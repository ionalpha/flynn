package editor_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// genEvent draws one editor-bound event: typed text (including wide and
// combining characters), an editing or movement key, an emacs binding, or a
// paste.
func genEvent(rt *rapid.T) input.Event {
	switch rapid.IntRange(0, 3).Draw(rt, "kind") {
	case 0:
		return input.Key{Code: rapid.RuneFrom([]rune("abc é😀_- ")).Draw(rt, "text")}
	case 1:
		codes := []rune{
			input.KeyBackspace, input.KeyDelete, input.KeyLeft, input.KeyRight,
			input.KeyUp, input.KeyDown, input.KeyHome, input.KeyEnd,
		}
		return input.Key{Code: rapid.SampledFrom(codes).Draw(rt, "code")}
	case 2:
		if rapid.Bool().Draw(rt, "alt") {
			return input.Key{
				Code: rapid.SampledFrom([]rune{'b', 'f', 'd', 'y', '_'}).Draw(rt, "altcode"),
				Mods: input.ModAlt,
			}
		}
		return input.Key{
			Code: rapid.SampledFrom([]rune{'a', 'e', 'b', 'f', 'd', 'k', 'u', 'w', 'y', 'j', '_'}).Draw(rt, "ctrlcode"),
			Mods: input.ModCtrl,
		}
	default:
		return input.Paste{Text: rapid.StringMatching("[a-z \n]{0,200}").Draw(rt, "paste")}
	}
}

// TestEditorInvariants drives a random event sequence and checks the
// properties every state must satisfy: rows never exceed the render width,
// the cursor lands inside the rendered rows, and Content never leaks a
// carriage return or an unexpanded chip label.
func TestEditorInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var e editor.Editor
		width := rapid.IntRange(1, 40).Draw(rt, "width")
		n := rapid.IntRange(0, 60).Draw(rt, "events")
		for i := 0; i < n; i++ {
			e.Handle(genEvent(rt))

			rows, curRow, curCol := e.Render(width)
			if len(rows) == 0 {
				rt.Fatalf("Render returned no rows")
			}
			for _, row := range rows {
				if w := ansi.StringWidth(row); w > width {
					rt.Fatalf("row %q is %d cells wide at width %d", row, w, width)
				}
			}
			if curRow < 0 || curRow >= len(rows) || curCol < 0 || curCol > width {
				rt.Fatalf("cursor (%d,%d) outside %d rows at width %d", curRow, curCol, len(rows), width)
			}
			if c := e.Content(); strings.Contains(c, "\r") || strings.Contains(c, "[paste #") {
				rt.Fatalf("Content leaked a raw CR or chip label: %q", c)
			}
		}
	})
}

// TestUndoRestoresContent checks that after any event sequence, unwinding
// the whole undo stack and replaying the redo stack round-trips the content.
func TestUndoRestoresContent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var e editor.Editor
		n := rapid.IntRange(0, 40).Draw(rt, "events")
		for i := 0; i < n; i++ {
			e.Handle(genEvent(rt))
		}
		final := e.Content()
		// The event sequence itself may include undo/redo keys, so count
		// the undos that actually fire and reverse exactly those.
		k := 0
		for e.Undo() {
			k++
		}
		for i := 0; i < k; i++ {
			e.Redo()
		}
		if got := e.Content(); got != final {
			rt.Fatalf("undo/redo round trip: %q, want %q", got, final)
		}
	})
}
