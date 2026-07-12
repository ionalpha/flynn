package editor_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// typeIn feeds text one key at a time, the way a user types it.
func typeIn(e *editor.Editor, s string) {
	for _, r := range s {
		e.Handle(input.Key{Code: r})
	}
}

func TestTypeAndContent(t *testing.T) {
	var e editor.Editor
	typeIn(&e, "hello world")
	if got := e.Content(); got != "hello world" {
		t.Fatalf("Content = %q, want %q", got, "hello world")
	}
}

func TestBackspaceDeletesWholeGrapheme(t *testing.T) {
	var e editor.Editor
	// A ZWJ family emoji is many runes but one cluster; one Backspace must
	// remove all of it.
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	e.Insert("x" + family)
	e.Backspace()
	if got := e.Content(); got != "x" {
		t.Fatalf("Content after backspace = %q, want %q", got, "x")
	}
}

func TestSmallPasteInline(t *testing.T) {
	var e editor.Editor
	e.Handle(input.Paste{Text: "ls -la"})
	if got := e.Content(); got != "ls -la" {
		t.Fatalf("Content = %q, want %q", got, "ls -la")
	}
}

func TestLargePasteBecomesChip(t *testing.T) {
	var e editor.Editor
	pasted := "line one\r\nline two\r\nline three"
	e.Insert("see: ")
	e.Handle(input.Paste{Text: pasted})

	// The chip renders as one short label, not the pasted lines.
	rows, _, _ := e.Render(80)
	if len(rows) != 1 || !strings.Contains(rows[0], "[paste #1: 3 lines]") {
		t.Fatalf("rendered rows = %q, want one row with the chip label", rows)
	}
	// Content expands it, with line endings normalized.
	want := "see: line one\nline two\nline three"
	if got := e.Content(); got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
	// One Backspace removes the whole chip.
	e.Backspace()
	if got := e.Content(); got != "see: " {
		t.Fatalf("Content after backspace = %q, want %q", got, "see: ")
	}
}

func TestImageChipIsAttachedNotText(t *testing.T) {
	var e editor.Editor
	e.Insert("look at ")
	e.InsertImage(editor.Attachment{MediaType: "image/png", Data: []byte("PNG-A")})
	e.Insert(" and this")

	// The image is not part of the prompt text; the chip contributes nothing.
	if got, want := e.Content(), "look at  and this"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
	// It renders as one short label alongside the text.
	rows, _, _ := e.Render(80)
	if len(rows) != 1 || !strings.Contains(rows[0], "[Image #1]") {
		t.Fatalf("rendered rows = %q, want the image chip label", rows)
	}
	// The bytes surface as an attachment.
	att := e.Attachments()
	if len(att) != 1 || string(att[0].Data) != "PNG-A" || att[0].MediaType != "image/png" {
		t.Fatalf("Attachments = %+v, want one image/png PNG-A", att)
	}
}

func TestImageOnlyBuffer(t *testing.T) {
	var e editor.Editor
	e.InsertImage(editor.Attachment{MediaType: "image/png", Data: []byte("only")})
	if got := e.Content(); got != "" {
		t.Fatalf("Content = %q, want empty for an image-only buffer", got)
	}
	if e.Empty() {
		t.Fatal("Empty() = true, want false: an image chip is buffer content")
	}
	if att := e.Attachments(); len(att) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(att))
	}
}

func TestBackspaceRemovesWholeImageChip(t *testing.T) {
	var e editor.Editor
	e.Insert("a")
	e.InsertImage(editor.Attachment{MediaType: "image/png", Data: []byte("x")})
	// One Backspace removes the whole chip, and its attachment goes with it.
	e.Backspace()
	if got := e.Content(); got != "a" {
		t.Fatalf("Content after backspace = %q, want %q", got, "a")
	}
	if att := e.Attachments(); att != nil {
		t.Fatalf("Attachments = %+v, want nil after the chip is deleted", att)
	}
}

func TestAttachmentsKeepBufferOrder(t *testing.T) {
	var e editor.Editor
	e.InsertImage(editor.Attachment{Data: []byte("first")})
	e.Insert(" then ")
	e.InsertImage(editor.Attachment{Data: []byte("second")})
	att := e.Attachments()
	if len(att) != 2 || string(att[0].Data) != "first" || string(att[1].Data) != "second" {
		t.Fatalf("Attachments = %+v, want first then second in buffer order", att)
	}
}

func TestEmptyImageIgnored(t *testing.T) {
	var e editor.Editor
	e.InsertImage(editor.Attachment{MediaType: "image/png"})
	if !e.Empty() {
		t.Fatal("an image with no bytes must not insert a chip")
	}
}

func TestKillYankYankPop(t *testing.T) {
	var e editor.Editor
	e.Insert("alpha")
	e.KillToStart() // ring: [alpha]
	e.Insert("beta")
	e.KillToStart() // ring: [beta alpha]
	e.Yank()
	if got := e.Content(); got != "beta" {
		t.Fatalf("after yank: %q, want %q", got, "beta")
	}
	e.YankPop()
	if got := e.Content(); got != "alpha" {
		t.Fatalf("after yank-pop: %q, want %q", got, "alpha")
	}
	e.YankPop() // cycles back around
	if got := e.Content(); got != "beta" {
		t.Fatalf("after second yank-pop: %q, want %q", got, "beta")
	}
}

func TestConsecutiveKillsCoalesce(t *testing.T) {
	var e editor.Editor
	e.Insert("one two three")
	e.KillWordBack()
	e.KillWordBack()
	e.Yank()
	if got := e.Content(); got != "one two three" {
		t.Fatalf("Content = %q, want the two kills yanked back as one", got)
	}
}

func TestUndoCoalescesTyping(t *testing.T) {
	var e editor.Editor
	typeIn(&e, "abc")
	e.Undo()
	if !e.Empty() {
		t.Fatalf("one undo should erase the whole typing run, got %q", e.Content())
	}
	e.Redo()
	if got := e.Content(); got != "abc" {
		t.Fatalf("redo = %q, want %q", got, "abc")
	}
}

func TestUndoRunsBreakAtEdits(t *testing.T) {
	var e editor.Editor
	typeIn(&e, "ab")
	e.Backspace()
	typeIn(&e, "c")
	e.Undo() // undoes "c"
	if got := e.Content(); got != "a" {
		t.Fatalf("after first undo: %q, want %q", got, "a")
	}
	e.Undo() // undoes the backspace
	if got := e.Content(); got != "ab" {
		t.Fatalf("after second undo: %q, want %q", got, "ab")
	}
}

func TestUpDownAndHistoryEdges(t *testing.T) {
	var e editor.Editor
	e.Insert("first\nsecond")
	if got := e.Handle(input.Key{Code: input.KeyDown}); got != editor.ActionHistoryNext {
		t.Fatalf("Down on last line = %v, want ActionHistoryNext", got)
	}
	if got := e.Handle(input.Key{Code: input.KeyUp}); got != editor.ActionRedraw {
		t.Fatalf("Up from second line = %v, want ActionRedraw", got)
	}
	if got := e.Handle(input.Key{Code: input.KeyUp}); got != editor.ActionHistoryPrev {
		t.Fatalf("Up on first line = %v, want ActionHistoryPrev", got)
	}
}

func TestUpKeepsColumn(t *testing.T) {
	var e editor.Editor
	e.Insert("abcdef\nxy")
	e.LineEnd() // end of "xy"
	e.Up()
	e.Insert("|")
	if got := e.Content(); got != "ab|cdef\nxy" {
		t.Fatalf("Content = %q, want cursor to land at column 2 of the longer line", got)
	}
}

func TestEnterSubmitsAndVariantsInsertNewline(t *testing.T) {
	var e editor.Editor
	typeIn(&e, "hi")
	if got := e.Handle(input.Key{Code: input.KeyEnter}); got != editor.ActionSubmit {
		t.Fatalf("Enter = %v, want ActionSubmit", got)
	}
	e.Handle(input.Key{Code: input.KeyEnter, Mods: input.ModAlt})
	e.Handle(input.Key{Code: 'j', Mods: input.ModCtrl})
	if got := e.Content(); got != "hi\n\n" {
		t.Fatalf("Content = %q, want two line breaks appended", got)
	}
}

func TestRenderWrapsAndPlacesCursor(t *testing.T) {
	var e editor.Editor
	e.Insert("abcdefgh")
	rows, curRow, curCol := e.Render(3)
	want := []string{"abc", "def", "gh"}
	if len(rows) != 3 || rows[0] != want[0] || rows[1] != want[1] || rows[2] != want[2] {
		t.Fatalf("rows = %q, want %q", rows, want)
	}
	if curRow != 2 || curCol != 2 {
		t.Fatalf("cursor = (%d,%d), want (2,2)", curRow, curCol)
	}
}

func TestRenderCursorAtFullRowWraps(t *testing.T) {
	var e editor.Editor
	e.Insert("abc")
	rows, curRow, curCol := e.Render(3)
	if len(rows) != 2 || curRow != 1 || curCol != 0 {
		t.Fatalf("rows=%q cursor=(%d,%d), want the cursor wrapped to row 1 col 0", rows, curRow, curCol)
	}
}

func TestTabsAndControlBytesNormalized(t *testing.T) {
	var e editor.Editor
	e.Insert("a\tb\x07c")
	if got := e.Content(); got != "a    b"+"c" {
		t.Fatalf("Content = %q, want tab expanded and the bell dropped", got)
	}
}

// Dropping a control byte must not splice the bytes around it into a rune
// that was not in the input. Here \xe4\x82 and \x83 are separated by a \x19;
// gluing them together makes a valid two-cell rune, and a row wrapped on the
// widths of the parts would then render wider than the width it was given.
func TestInvalidBytesNeverSpliceIntoARune(t *testing.T) {
	var e editor.Editor
	e.Insert("ab\xe4\x82\x19\x83cd")
	if got := e.Content(); got != "abcd" {
		t.Fatalf("Content = %q, want the invalid bytes dropped, not spliced", got)
	}
	rows, _, _ := e.Render(6)
	if len(rows) != 1 || ansi.StringWidth(rows[0]) != 4 {
		t.Fatalf("rows = %q, want one 4-cell row", rows)
	}
}

// A cluster wider than the whole terminal is clipped away, not kept. Render
// and clip must measure with the same function: uniseg's width table and
// ansi's disagree on some recently assigned runes, and a cluster clip thinks
// fits but the caller counts as wider is a row that escapes its width.
func TestRuneWiderThanTerminalIsClipped(t *testing.T) {
	var e editor.Editor
	e.Insert(string(rune(0x1FAC6))) // two cells wide
	rows, _, _ := e.Render(1)
	for _, row := range rows {
		if w := ansi.StringWidth(row); w > 1 {
			t.Fatalf("row %q is %d cells wide at width 1", row, w)
		}
	}
}

// A C1 control is an escape introducer to a terminal and has no cell width,
// so it never reaches the buffer.
func TestC1ControlsDropped(t *testing.T) {
	var e editor.Editor
	csi := string(rune(0x9b)) // CSI: a terminal reads it as an escape introducer
	e.Insert("a" + csi + "b")
	if got := e.Content(); got != "ab" {
		t.Fatalf("Content = %q, want the C1 control dropped", got)
	}
}

// An atom's width is not a property of the atom. A variation selector fuses
// with the digit before it into a two-cell emoji, so a row full of digits
// with selectors between them is wider than the digits are. The wrap must
// read the row's growth, not the sum of the parts.
func TestVariationSelectorWidensItsNeighbour(t *testing.T) {
	var e editor.Editor
	vs16 := string(rune(0xFE0F)) // emoji presentation selector
	for range 12 {
		e.Insert("0")
		e.Insert(vs16)
	}
	rows, _, _ := e.Render(20)
	for _, row := range rows {
		if w := ansi.StringWidth(row); w > 20 {
			t.Fatalf("row %q is %d cells wide at width 20", row, w)
		}
	}
}

func TestClear(t *testing.T) {
	var e editor.Editor
	e.Handle(input.Paste{Text: "x\ny"})
	e.Clear()
	if !e.Empty() || e.Content() != "" {
		t.Fatalf("Clear left content %q", e.Content())
	}
}

// TestCtrlDEmptyBufferUnclaimed proves Ctrl+D follows the readline EOF
// convention: on an empty buffer the key is left unclaimed for the session,
// while on a non-empty buffer it deletes forward as usual.
func TestCtrlDEmptyBufferUnclaimed(t *testing.T) {
	var e editor.Editor
	if got := e.Handle(input.Key{Code: 'd', Mods: input.ModCtrl}); got != editor.ActionNone {
		t.Fatalf("Ctrl+D on an empty buffer = %v, want ActionNone", got)
	}
	typeIn(&e, "ab")
	e.Handle(input.Key{Code: input.KeyLeft})
	if got := e.Handle(input.Key{Code: 'd', Mods: input.ModCtrl}); got != editor.ActionRedraw {
		t.Fatalf("Ctrl+D mid-buffer = %v, want ActionRedraw", got)
	}
	if got := e.Content(); got != "a" {
		t.Fatalf("Ctrl+D did not delete forward: content = %q, want %q", got, "a")
	}
}
