package editor_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/input"
)

// FuzzEditor pipes arbitrary terminal bytes through the real input decoder
// into the editor, which is exactly the production path: whatever a hostile
// pty makes the decoder emit, the editor must accept without panicking,
// keep every rendered row inside the width, and keep the cursor inside the
// rows.
func FuzzEditor(f *testing.F) {
	f.Add([]byte("hello world\x1b[D\x1b[D\x7f"), 10)
	f.Add([]byte("\x17\x0b\x19\x19\x1by"), 20)
	f.Add([]byte("\x1b[200~two\nlines\x1b[201~\x7f"), 5)
	f.Add([]byte("a\x01b\x05\x0bc\x15\x19\x1b_"), 3)
	f.Add([]byte("é日🙂\x1b[H\x1b[3~"), 1)

	f.Fuzz(func(t *testing.T, data []byte, width int) {
		if width < 1 || width > 200 {
			width = 20
		}
		var d input.Decoder
		var e editor.Editor
		evs := d.Feed(data)
		evs = append(evs, d.Flush()...)
		for _, ev := range evs {
			e.Handle(ev)
		}
		rows, curRow, curCol := e.Render(width)
		if len(rows) == 0 {
			t.Fatalf("Render returned no rows for %q", data)
		}
		for _, row := range rows {
			if w := ansi.StringWidth(row); w > width {
				t.Fatalf("row %q is %d cells wide at width %d (input %q)", row, w, width, data)
			}
		}
		if curRow < 0 || curRow >= len(rows) || curCol < 0 || curCol > width {
			t.Fatalf("cursor (%d,%d) outside %d rows at width %d (input %q)", curRow, curCol, len(rows), width, data)
		}
		if strings.Contains(e.Content(), "\r") {
			t.Fatalf("Content leaked a raw CR from %q", data)
		}
	})
}
