package editor

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Render lays the buffer out as rows no wider than width and reports the
// cursor's cell position. Logical lines soft-wrap. Rows are plain text; the
// caller styles them, which keeps width arithmetic exact.
//
// Each atom is measured as it lands in the row. A cluster can join the one before
// it (a variation selector turns the preceding digit into a two-cell emoji, a
// combining mark rides its base), so the pair is not always as wide as the
// parts, and summing per-atom widths would overrun the terminal. The wrap
// decision reads the row's growth instead.
func (e *Editor) Render(width int) (rows []string, curRow, curCol int) {
	if width < 1 {
		width = 1
	}
	var row []byte
	col := 0
	endRow := func() {
		rows = append(rows, string(row))
		row = row[:0]
		col = 0
	}
	// grow is the cells gained by appending text, which differs from its own
	// width when the ends fuse into one cluster.
	grow := func(text string) int {
		return ansi.StringWidth(string(row)+text) - col
	}
	for i, a := range e.atoms {
		if i == e.cur {
			curRow, curCol = len(rows), col
		}
		if a.text == "\n" {
			endRow()
			continue
		}
		text := a.text
		if ansi.StringWidth(text) > width {
			// A chip label can exceed a very narrow window.
			text, _ = clip(text, width)
		}
		w := grow(text)
		if col+w > width {
			endRow()
			// On an empty row nothing can fuse.
			w = ansi.StringWidth(text)
		}
		if i == e.cur && col == 0 {
			// The atom wrapped, so the cursor sits at the wrap point.
			curRow, curCol = len(rows), 0
		}
		row = append(row, text...)
		col += w
	}
	if e.cur == len(e.atoms) {
		if col >= width {
			endRow()
		}
		curRow, curCol = len(rows), col
	}
	endRow()
	return rows, curRow, curCol
}

// clip truncates text to at most width cells, whole clusters only. It measures
// with ansi.StringWidth, as Render and the painter do: uniseg's table can
// disagree on a recently assigned rune, and a clip that keeps a cluster the
// caller counts as wider lets a row escape its width.
func clip(text string, width int) (string, int) {
	var out strings.Builder
	used := 0
	for text != "" {
		var cluster string
		cluster, text, _, _ = uniseg.FirstGraphemeClusterInString(text, -1)
		w := ansi.StringWidth(cluster)
		if used+w > width {
			break
		}
		out.WriteString(cluster)
		used += w
	}
	return out.String(), used
}
