// Package editor is the composer's line editor: the multi-line buffer the
// user types a prompt into, with grapheme-correct cursor movement, a kill
// ring, undo, and atomic paste chips.
//
// The buffer is a sequence of atoms. An atom is either one grapheme cluster
// (so an emoji ZWJ sequence or a combining accent moves and deletes as one
// unit, never splitting into broken runes) or a paste chip: a large paste is
// held out of the buffer and represented by a single atom that the cursor
// steps over and Backspace removes whole, so a 400-line paste occupies one
// cell of editing surface instead of drowning the composer. Content expands
// chips back to their full text at submit time.
//
// The editor is pure state: no terminal, no reader, no timers. It consumes
// the input package's decoded events through Handle and reports what the
// caller should do (redraw, submit, recall history), and it renders itself
// to width-bounded rows for the screen painter. Determinism holds by
// construction: undo grouping keys off edit boundaries, not wall-clock
// idle time.
package editor

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

// chipThreshold is the paste size, in grapheme clusters, above which a
// single-line paste becomes a chip instead of inline text. Any paste with a
// newline becomes a chip regardless: inline multi-line pastes are the classic
// way a composer turns into an unreadable wall.
const chipThreshold = 120

// maxUndo bounds the undo stack. Beyond it the oldest snapshot is dropped;
// the composer's working set is small enough that snapshots stay cheap.
const maxUndo = 200

// maxRing bounds the kill ring.
const maxRing = 16

// atom is one editing unit: a grapheme cluster, a paste chip when chip is
// nonzero, or an image chip when img is nonzero. For either chip kind text
// carries the display label; the payload is held out of the buffer (chips in
// the chips map, images in the images map). chip and img are never both set.
type atom struct {
	text string
	chip int
	img  int
}

// opaque reports whether the atom is a chip (paste or image) rather than a
// grapheme cluster: it moves, deletes, and bounds a completion token as one
// unit, and is never part of a word being typed.
func (a atom) opaque() bool { return a.chip != 0 || a.img != 0 }

// Attachment is one image bound to the composer: the encoded bytes and their
// IANA media type. The clipboard port yields PNG; the value is what a submit
// carries alongside the prompt text so the image reaches the model. It is
// deliberately a plain UI-layer type, not the model port's llm.Image, so the
// reusable composer stays free of a model-port dependency; the host converts
// it at the one point where the prompt is turned into a turn.
type Attachment struct {
	MediaType string
	Data      []byte
}

// snapshot is one undo/redo state.
type snapshot struct {
	atoms []atom
	cur   int
}

// lastOp tracks what the previous mutation was, which drives undo
// coalescing (runs of typing collapse into one undo step), kill coalescing
// (consecutive kills build one ring entry, as in emacs), and yank-pop
// eligibility.
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

// Content returns the buffer's text with every paste chip expanded to its
// full pasted text. Image chips contribute nothing: an image travels with the
// turn as a separate attachment (see Attachments), not as text. This is the
// prompt text submit sends.
func (e *Editor) Content() string {
	var b strings.Builder
	for _, a := range e.atoms {
		switch {
		case a.chip != 0:
			b.WriteString(e.chips[a.chip])
		case a.img != 0:
			// The image rides as an attachment, not inline text.
		default:
			b.WriteString(a.text)
		}
	}
	return b.String()
}

// Attachments returns the images bound to the composer in buffer order, so
// the order the model sees matches the order the chips appear and a chip the
// user backspaced away is absent. It returns nil when there are none.
func (e *Editor) Attachments() []Attachment {
	var out []Attachment
	for _, a := range e.atoms {
		if a.img != 0 {
			out = append(out, e.images[a.img])
		}
	}
	return out
}

// Clear resets the buffer, history, and chips for the next prompt.
func (e *Editor) Clear() {
	*e = Editor{}
}

// Insert splices text at the cursor. Line endings are normalized to \n,
// tabs become spaces (a tab's cell width depends on its column, which would
// make row widths unstable under the painter's overflow guard), and other
// control characters are dropped.
func (e *Editor) Insert(s string) {
	as := graphemes(s)
	if len(as) == 0 {
		return
	}
	// A run of single-cluster insertions coalesces into one undo step; the
	// first keystroke of the run takes the snapshot.
	if e.last != opInsert || len(as) != 1 {
		e.pushUndo()
	}
	e.splice(as)
	e.last = opInsert
}

// InsertPaste inserts one paste event: inline when small and single-line,
// as an atomic chip otherwise.
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

// InsertImage binds an image to the composer and inserts an atomic chip for
// it at the cursor: like a paste chip, the cursor steps over it whole and
// Backspace removes it whole, and it holds no bytes in the buffer. The image
// itself is carried out of line and surfaced by Attachments at submit.
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

// imageLabel is an image chip's display text. Plain ASCII brackets, matching
// the paste chip, so the label survives every emulator in the validation
// matrix. The number is the chip's position among inserted images, not its
// buffer order, so it stays stable as earlier text is edited.
func imageLabel(id int) string {
	return fmt.Sprintf("[Image #%d]", id)
}

// chipLabel is the chip's display text. Plain ASCII brackets on purpose:
// the label must survive every emulator in the validation matrix, ConPTY
// included.
func chipLabel(id int, text string) string {
	if n := strings.Count(text, "\n") + 1; n > 1 {
		return fmt.Sprintf("[paste #%d: %d lines]", id, n)
	}
	return fmt.Sprintf("[paste #%d: %d chars]", id, uniseg.GraphemeClusterCount(text))
}

// Backspace deletes the atom before the cursor: one grapheme cluster, or a
// whole chip.
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

// Left moves the cursor one atom back; atom granularity is what makes chips
// and grapheme clusters atomic to the user.
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

// isWord reports whether the atom belongs to a word (letter, digit, or
// underscore; a chip counts as one word).
func isWord(a atom) bool {
	if a.opaque() {
		return true
	}
	for _, r := range a.text {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}
	return false
}

// wordLeft returns the cursor position one word back: skip separators, then
// the word itself.
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

// wordRight returns the cursor position one word forward.
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

// lineBounds returns the buffer indices of the current logical line:
// start is just after the previous newline atom, end is at the next one.
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

// Up moves the cursor to the previous logical line, keeping the column when
// it fits. It reports false when the cursor is already on the first line,
// which is the caller's cue to recall prompt history instead.
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

// Down mirrors Up toward the next line; false on the last line means the
// caller should move forward in prompt history.
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

// kill removes atoms[from:to] and adds them to the kill ring. Consecutive
// kills in the same direction grow one ring entry (forward kills append,
// backward kills prepend), so Ctrl+W Ctrl+W Ctrl+Y restores both words.
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

// KillToEnd kills from the cursor to the end of the line, or the newline
// itself when the cursor already sits at the end (emacs Ctrl+K).
func (e *Editor) KillToEnd() {
	_, end := e.lineBounds()
	if end == e.cur && end < len(e.atoms) {
		end++
	}
	e.kill(e.cur, end, false)
}

// KillToStart kills from the start of the line to the cursor.
func (e *Editor) KillToStart() {
	start, _ := e.lineBounds()
	e.kill(start, e.cur, true)
}

// KillWordBack kills the word before the cursor.
func (e *Editor) KillWordBack() {
	e.kill(e.wordLeft(), e.cur, true)
}

// KillWordForward kills the word after the cursor.
func (e *Editor) KillWordForward() {
	e.kill(e.cur, e.wordRight(), false)
}

// Yank inserts the most recent kill at the cursor.
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

// YankPop replaces the text a yank just inserted with the next older ring
// entry, cycling through the ring. It applies only immediately after a yank.
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

// Undo restores the previous snapshot, reporting whether there was one.
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

// Redo reverses an Undo, reporting whether there was one to reverse.
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

// pushUndo records the current state before a mutation and invalidates the
// redo stack, as any new edit does.
func (e *Editor) pushUndo() {
	e.undo = append(e.undo, e.snap())
	if len(e.undo) > maxUndo {
		e.undo = e.undo[1:]
	}
	e.redo = nil
}

// splice inserts atoms at the cursor and advances past them.
func (e *Editor) splice(as []atom) {
	e.atoms = append(e.atoms[:e.cur], append(as, e.atoms[e.cur:]...)...)
	e.cur += len(as)
}

// graphemes segments text into atoms, normalizing line endings and dropping
// control characters that have no stable cell width.
func graphemes(s string) []atom {
	s = normalizeNewlines(s)
	s = strings.ReplaceAll(s, "\t", "    ")
	var as []atom
	for s != "" {
		var cluster string
		cluster, s, _, _ = uniseg.FirstGraphemeClusterInString(s, -1)
		if r := []rune(cluster)[0]; r < 0x20 && r != '\n' || r == 0x7f {
			continue
		}
		as = append(as, atom{text: cluster})
	}
	return as
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
