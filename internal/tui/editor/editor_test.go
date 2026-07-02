package editor_test

import (
	"strings"
	"testing"

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

func TestClear(t *testing.T) {
	var e editor.Editor
	e.Handle(input.Paste{Text: "x\ny"})
	e.Clear()
	if !e.Empty() || e.Content() != "" {
		t.Fatalf("Clear left content %q", e.Content())
	}
}
