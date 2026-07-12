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
//
// Each atom is measured as it lands in the row, not on its own. An atom's
// width is not a property of the atom: a cluster can join the one before it
// (a variation selector turns the digit ahead of it into a two-cell emoji, a
// combining mark rides its base) and the pair is not always as wide as the
// parts. Summing per-atom widths overruns the terminal for exactly those
// inputs, so the growth of the row is what the wrap decision reads.
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
	// grow reports the cells the row gains by appending text, which is not
	// text's own width when the two ends fuse into one cluster.
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
			// An atom wider than the terminal (a chip label on a very
			// narrow window) is clipped rather than allowed to overflow.
			text, _ = clip(text, width)
		}
		w := grow(text)
		if col+w > width {
			endRow()
			// On an empty row nothing can fuse, so the atom costs its own
			// width, and clip has already bounded that by the width.
			w = ansi.StringWidth(text)
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
//
// It measures with ansi.StringWidth, the same function Render and the
// painter measure with. uniseg reports a width of its own here, but the two
// tables can disagree on a recently assigned rune, and a clip that keeps a
// cluster the caller then counts as wider is how a row escapes its width.
// One width authority, or the arithmetic is only approximately true.
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
