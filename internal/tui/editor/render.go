package editor

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Render lays the buffer out as width-bounded rows for the screen painter
// and reports the cursor's cell position within them. Logical lines soft-wrap
// at the width; no row ever exceeds it, matching the painter's overflow
// guard. Rows are plain text: styling (the chip highlight, the prompt
// gutter) is the caller's composition step, and keeping ANSI out of the
// editor keeps width arithmetic exact.
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
	for i, a := range e.atoms {
		if i == e.cur {
			curRow, curCol = len(rows), col
		}
		if a.text == "\n" {
			endRow()
			continue
		}
		text, w := a.text, ansi.StringWidth(a.text)
		if w > width {
			// An atom wider than the terminal (a chip label on a very
			// narrow window) is clipped rather than allowed to overflow.
			text, w = clip(text, width)
		}
		if col+w > width {
			endRow()
		}
		if i == e.cur && col == 0 {
			// The atom wrapped, so the cursor sits at the wrap point,
			// not at the end of the previous row.
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

// clip truncates text to at most width cells, whole grapheme clusters only.
func clip(text string, width int) (string, int) {
	var out strings.Builder
	used := 0
	for text != "" {
		var cluster string
		var w int
		cluster, text, w, _ = uniseg.FirstGraphemeClusterInString(text, -1)
		if used+w > width {
			break
		}
		out.WriteString(cluster)
		used += w
	}
	return out.String(), used
}
