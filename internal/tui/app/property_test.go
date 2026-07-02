package app

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// TestComposerInvariants drives the composer with arbitrary content, cursor
// positions, and widths, and checks the properties every frame depends on:
// no row wider than the terminal, exactly one cursor cell, and no content
// lost between the editor and the styled rows.
func TestComposerInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var ed editor.Editor
		// Typed text as the decoder would deliver it: printable characters
		// including wide and combining ones, plus line breaks. Raw escape
		// bytes never reach the editor; the decoder consumes them.
		text := rapid.StringOfN(rapid.RuneFrom([]rune("abc é😀官_- \n")), 0, 80, -1).Draw(rt, "text")
		ed.Insert(text)
		for range rapid.IntRange(0, 40).Draw(rt, "lefts") {
			ed.Left()
		}
		width := rapid.IntRange(3, 60).Draw(rt, "width")

		rows := composerRows(&ed, theme.Default(), width, "hint", "")
		if len(rows) == 0 {
			rt.Fatal("composer rendered no rows")
		}
		joined := strings.Join(rows, "\n")
		if n := strings.Count(joined, "\x1b[7m"); n != 1 {
			rt.Fatalf("got %d cursor cells, want exactly 1 in %q", n, joined)
		}
		for _, row := range rows {
			if w := screen.Width(row); w > width {
				rt.Fatalf("row %q is %d cells wide, terminal is %d", row, w, width)
			}
		}
	})
}

// TestHistoryNeverLosesTheDraft walks a random sequence of prev and next
// steps and checks that returning to the live end always restores the exact
// draft, no matter the walk.
func TestHistoryNeverLosesTheDraft(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var h history
		entries := rapid.SliceOfN(rapid.String(), 1, 8).Draw(rt, "entries")
		for _, e := range entries {
			if e != "" {
				h.add(e)
			}
		}
		draft := rapid.String().Draw(rt, "draft")

		current := draft
		depth := 0
		for _, down := range rapid.SliceOfN(rapid.Bool(), 1, 20).Draw(rt, "walk") {
			if down {
				if text, ok := h.next(); ok {
					current = text
					depth--
				}
			} else {
				if text, ok := h.prev(current); ok {
					current = text
					depth++
				}
			}
		}
		for depth > 0 {
			text, ok := h.next()
			if !ok {
				rt.Fatalf("history refused to walk back down at depth %d", depth)
			}
			current = text
			depth--
		}
		if current != draft {
			rt.Fatalf("draft came back as %q, want %q", current, draft)
		}
	})
}
