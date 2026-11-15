// Package editor is the composer's multi-line line editor, with
// grapheme-correct cursor movement, a kill ring, undo and atomic paste chips.
//
// The buffer is a sequence of atoms. An atom is one grapheme cluster (an emoji
// ZWJ sequence or a combining accent moves and deletes as one unit) or a chip: a
// large paste held out of the buffer and shown as one atom that the cursor steps
// over and Backspace removes whole. Content expands chips back to full text at
// submit.
//
// The editor is pure state: no terminal, reader or timers. It consumes decoded
// input events through Handle, reports what the caller should do (redraw,
// submit, recall history) and renders width-bounded rows. Undo grouping keys off
// edit boundaries, not idle time, so behavior is deterministic.
package editor

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// chipThreshold is the size in grapheme clusters above which a single-line
// paste becomes a chip. Any paste containing a newline is a chip regardless.
const chipThreshold = 120

// maxUndo bounds the undo stack; the oldest snapshot is dropped beyond it.
const maxUndo = 200

// maxRing bounds the kill ring.
const maxRing = 16

// atom is one editing unit: a grapheme cluster, a paste chip (chip nonzero) or
// an image chip (img nonzero, never both). For a chip, text is the display label
// and the payload sits in the chips or images map.
type atom struct {
	text string
	chip int
	img  int
}

// opaque reports whether the atom is a chip, which moves, deletes and bounds a
// completion token as one unit.
func (a atom) opaque() bool { return a.chip != 0 || a.img != 0 }

// Attachment is an image bound to the composer: encoded bytes and IANA media
// type. It is a plain UI type rather than the model port's image so the
// composer has no model-port dependency; the host converts it when the prompt
// becomes a turn.
type Attachment struct {
	MediaType string
	Data      []byte
}

// snapshot is one undo/redo state.
type snapshot struct {
	atoms []atom
	cur   int
}

// lastOp is the previous mutation. It drives undo coalescing (typing runs
// collapse into one step), kill coalescing (consecutive kills build one ring
// entry, as in emacs) and yank-pop eligibility.
type lastOp int

const (
	opNone lastOp = iota
	opInsert
	opKillFwd
	opKillBack
	opYank
)

// Editor is the multi-line composer buffer. The zero value is ready to use.
type Editor struct {
	atoms []atom
	cur   int

	chips    map[int]string
	nextChip int

	images  map[int]Attachment
	nextImg int

	ring   [][]atom // most recent kill first
	ringAt int      // which ring entry the last yank inserted
	yankAt int      // where the last yank inserted, for yank-pop replacement
	yankN  int      // how many atoms the last yank inserted

	undo []snapshot
	redo []snapshot
	last lastOp

	keys Keymap // nil selects the default keymap
}

// Empty reports whether the buffer holds nothing.
func (e *Editor) Empty() bool { return len(e.atoms) == 0 }

// Content returns the prompt text with paste chips expanded. Image chips
// contribute nothing; see [Editor.Attachments].
func (e *Editor) Content() string {
	var b strings.Builder
	for _, a := range e.atoms {
		switch {
		case a.chip != 0:
			b.WriteString(e.chips[a.chip])
		case a.img != 0:
		default:
			b.WriteString(a.text)
		}
	}
	return b.String()
}

// Attachments returns the images in buffer order, omitting chips the user
// deleted. It returns nil when there are none.
func (e *Editor) Attachments() []Attachment {
	var out []Attachment
	for _, a := range e.atoms {
		if a.img != 0 {
			out = append(out, e.images[a.img])
		}
	}
	return out
}

// Clear resets the buffer, undo history and chips.
func (e *Editor) Clear() {
	*e = Editor{}
}

// Insert splices text at the cursor. Line endings become \n, tabs become spaces
// (a tab's width depends on its column, which would destabilize row widths) and
// other control characters are dropped.
func (e *Editor) Insert(s string) {
	as := graphemes(s)
	if len(as) == 0 {
		return
	}
	// A run of single-cluster insertions is one undo step, snapshotted at its first.
	if e.last != opInsert || len(as) != 1 {
		e.pushUndo()
	}
	e.splice(as)
	e.last = opInsert
}

// InsertPaste inserts a paste inline when small and single-line, else as a chip.
func (e *Editor) InsertPaste(text string) {
	text = normalizeNewlines(text)
	if text == "" {
		return
	}
	if !strings.Contains(text, "\n") && uniseg.GraphemeClusterCount(text) <= chipThreshold {
		e.Insert(text)
		return
	}
	e.pushUndo()
	if e.chips == nil {
		e.chips = make(map[int]string)
	}
	e.nextChip++
	id := e.nextChip
	e.chips[id] = text
	e.splice([]atom{{text: chipLabel(id, text), chip: id}})
	e.last = opNone
}

// InsertImage binds an image and inserts an atomic chip for it at the cursor.
// The bytes stay out of the buffer and surface through [Editor.Attachments].
func (e *Editor) InsertImage(att Attachment) {
	if len(att.Data) == 0 {
		return
	}
	e.pushUndo()
	if e.images == nil {
		e.images = make(map[int]Attachment)
	}
	e.nextImg++
	id := e.nextImg
	e.images[id] = att
	e.splice([]atom{{text: imageLabel(id), img: id}})
	e.last = opNone
}

// imageLabel uses plain ASCII brackets so it survives every emulator. The number
// counts insertions, not buffer position, so it is stable under edits.
func imageLabel(id int) string {
	return fmt.Sprintf("[Image #%d]", id)
}

// chipLabel uses plain ASCII brackets so it survives every emulator, ConPTY
// included.
func chipLabel(id int, text string) string {
	if n := strings.Count(text, "\n") + 1; n > 1 {
		return fmt.Sprintf("[paste #%d: %d lines]", id, n)
	}
	return fmt.Sprintf("[paste #%d: %d chars]", id, uniseg.GraphemeClusterCount(text))
}

// Backspace deletes the atom before the cursor.
func (e *Editor) Backspace() {
	if e.cur == 0 {
		return
	}
	e.pushUndo()
	e.atoms = append(e.atoms[:e.cur-1], e.atoms[e.cur:]...)
	e.cur--
	e.last = opNone
}

// Delete deletes the atom under the cursor.
func (e *Editor) Delete() {
	if e.cur >= len(e.atoms) {
		return
	}
	e.pushUndo()
	e.atoms = append(e.atoms[:e.cur], e.atoms[e.cur+1:]...)
	e.last = opNone
}

// Left moves the cursor one atom back.
func (e *Editor) Left() {
	if e.cur > 0 {
		e.cur--
	}
	e.last = opNone
}

// Right moves the cursor one atom forward.
func (e *Editor) Right() {
	if e.cur < len(e.atoms) {
		e.cur++
	}
	e.last = opNone
}

// isWord reports a letter, digit or underscore; a chip counts as a word.
func isWord(a atom) bool {
	if a.opaque() {
		return true
	}
	for _, r := range a.text {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}
	return false
}

// wordLeft skips separators, then the word.
func (e *Editor) wordLeft() int {
	i := e.cur
	for i > 0 && !isWord(e.atoms[i-1]) {
		i--
	}
	for i > 0 && isWord(e.atoms[i-1]) {
		i--
	}
	return i
}

func (e *Editor) wordRight() int {
	i := e.cur
	for i < len(e.atoms) && !isWord(e.atoms[i]) {
		i++
	}
	for i < len(e.atoms) && isWord(e.atoms[i]) {
		i++
	}
	return i
}

// WordLeft moves the cursor to the start of the previous word.
func (e *Editor) WordLeft() { e.cur = e.wordLeft(); e.last = opNone }

// WordRight moves the cursor past the end of the next word.
func (e *Editor) WordRight() { e.cur = e.wordRight(); e.last = opNone }

// lineBounds returns the current logical line: start follows the previous
// newline atom, end is the next one.
func (e *Editor) lineBounds() (start, end int) {
	start = e.cur
	for start > 0 && e.atoms[start-1].text != "\n" {
		start--
	}
	end = e.cur
	for end < len(e.atoms) && e.atoms[end].text != "\n" {
		end++
	}
	return start, end
}

// LineStart moves the cursor to the start of the current logical line.
func (e *Editor) LineStart() {
	e.cur, _ = e.lineBounds()
	e.last = opNone
}

// LineEnd moves the cursor to the end of the current logical line.
func (e *Editor) LineEnd() {
	_, e.cur = e.lineBounds()
	e.last = opNone
}

// Up moves to the previous logical line, keeping the column when it fits. False
// on the first line is the cue to recall prompt history.
func (e *Editor) Up() bool {
	start, _ := e.lineBounds()
	if start == 0 {
		return false
	}
	col := e.cur - start
	prevEnd := start - 1 // the newline atom
	prevStart := prevEnd
	for prevStart > 0 && e.atoms[prevStart-1].text != "\n" {
		prevStart--
	}
	e.cur = min(prevStart+col, prevEnd)
	e.last = opNone
	return true
}

// Down mirrors Up; false on the last line is the cue to move forward in history.
func (e *Editor) Down() bool {
	start, end := e.lineBounds()
	if end == len(e.atoms) {
		return false
	}
	col := e.cur - start
	nextStart := end + 1
	nextEnd := nextStart
	for nextEnd < len(e.atoms) && e.atoms[nextEnd].text != "\n" {
		nextEnd++
	}
	e.cur = min(nextStart+col, nextEnd)
	e.last = opNone
	return true
}

// kill moves atoms[from:to] to the kill ring. Consecutive kills in one direction
// grow one entry (forward appends, backward prepends), so Ctrl+W Ctrl+W Ctrl+Y
// restores both words.
func (e *Editor) kill(from, to int, back bool) {
	if from >= to {
		e.last = opNone
		return
	}
	e.pushUndo()
	killed := append([]atom(nil), e.atoms[from:to]...)
	e.atoms = append(e.atoms[:from], e.atoms[to:]...)
	e.cur = from

	op := opKillFwd
	if back {
		op = opKillBack
	}
	if (e.last == opKillFwd || e.last == opKillBack) && len(e.ring) > 0 {
		if back {
			e.ring[0] = append(killed, e.ring[0]...)
		} else {
			e.ring[0] = append(e.ring[0], killed...)
		}
	} else {
		e.ring = append([][]atom{killed}, e.ring...)
		if len(e.ring) > maxRing {
			e.ring = e.ring[:maxRing]
		}
	}
	e.last = op
}

// KillToEnd kills to the end of the line, or the newline when already there
// (emacs Ctrl+K).
func (e *Editor) KillToEnd() {
	_, end := e.lineBounds()
	if end == e.cur && end < len(e.atoms) {
		end++
	}
	e.kill(e.cur, end, false)
}

// KillToStart deletes to the start of the line and saves it for Yank.
func (e *Editor) KillToStart() {
	start, _ := e.lineBounds()
	e.kill(start, e.cur, true)
}

// KillWordBack deletes the word before the cursor and saves it for Yank.
func (e *Editor) KillWordBack() {
	e.kill(e.wordLeft(), e.cur, true)
}

// KillWordForward deletes to the end of the word and saves it for Yank.
func (e *Editor) KillWordForward() {
	e.kill(e.cur, e.wordRight(), false)
}

// Yank inserts the kill buffer at the cursor.
func (e *Editor) Yank() {
	if len(e.ring) == 0 {
		e.last = opNone
		return
	}
	e.pushUndo()
	e.ringAt = 0
	e.yankAt = e.cur
	e.yankN = len(e.ring[0])
	e.splice(append([]atom(nil), e.ring[0]...))
	e.last = opYank
}

// YankPop replaces the text just yanked with the next older ring entry. It
// applies only immediately after a yank.
func (e *Editor) YankPop() {
	if e.last != opYank || len(e.ring) < 2 {
		e.last = opNone
		return
	}
	e.ringAt = (e.ringAt + 1) % len(e.ring)
	entry := e.ring[e.ringAt]
	e.atoms = append(e.atoms[:e.yankAt], e.atoms[e.yankAt+e.yankN:]...)
	e.cur = e.yankAt
	e.yankN = len(entry)
	e.splice(append([]atom(nil), entry...))
	e.last = opYank
}

// Undo reverts the last edit and reports whether there was one.
func (e *Editor) Undo() bool {
	if len(e.undo) == 0 {
		return false
	}
	e.redo = append(e.redo, e.snap())
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.restore(s)
	return true
}

// Redo reapplies the last undone edit and reports whether there was one.
func (e *Editor) Redo() bool {
	if len(e.redo) == 0 {
		return false
	}
	e.undo = append(e.undo, e.snap())
	s := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	e.restore(s)
	return true
}

func (e *Editor) snap() snapshot {
	return snapshot{atoms: append([]atom(nil), e.atoms...), cur: e.cur}
}

func (e *Editor) restore(s snapshot) {
	e.atoms = s.atoms
	e.cur = min(s.cur, len(s.atoms))
	e.last = opNone
}

// pushUndo snapshots before a mutation and invalidates the redo stack.
func (e *Editor) pushUndo() {
	e.undo = append(e.undo, e.snap())
	if len(e.undo) > maxUndo {
		e.undo = e.undo[1:]
	}
	e.redo = nil
}

func (e *Editor) splice(as []atom) {
	e.atoms = append(e.atoms[:e.cur], append(as, e.atoms[e.cur:]...)...)
	e.cur += len(as)
}

// graphemes segments text into atoms, dropping control characters and invalid
// UTF-8.
//
// Every atom must measure the same alone as inside a row, because Render sums
// per-atom widths to wrap. A pty can send a truncated or interleaved sequence,
// and dropping the controls splices the survivors into a rune never in the
// input: \xe4 \x19 \x82 \x83 loses the \x19 and leaves a valid two-cell rune built
// from three atoms that each measured one, so the row renders wider than it was
// wrapped. Bytes that cannot stand alone as a cluster do not become atoms.
func graphemes(s string) []atom {
	s = normalizeNewlines(s)
	s = strings.ReplaceAll(s, "\t", "    ")
	var as []atom
	for s != "" {
		var cluster string
		cluster, s, _, _ = uniseg.FirstGraphemeClusterInString(s, -1)
		if !utf8.ValidString(cluster) {
			continue
		}
		if r := []rune(cluster)[0]; isControl(r) && r != '\n' {
			continue
		}
		as = append(as, atom{text: cluster})
	}
	return as
}

// isControl reports a C0 or C1 control or DEL. They have no stable cell width,
// and a terminal reads C1 as escape introducers.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
