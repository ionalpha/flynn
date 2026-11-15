package app

import (
	"strings"

	"github.com/rivo/uniseg"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// The composer's gutter: a prompt marker on the first row, alignment padding
// on continuation rows.
const (
	gutterPrompt = "> "
	gutterCont   = "  "
	gutterWidth  = 2
)

// composerRows renders the prompt editor as styled rows. The hardware cursor is
// hidden (the painter rests it at the start of the last live row), so the
// visible cursor is a reverse-video cell spliced in at the editor's position. A
// non-empty marker replaces the first row's prompt marker, fitted to the gutter
// width.
func composerRows(ed *editor.Editor, th *theme.Theme, width int, placeholder, marker string) []string {
	inner := width - gutterWidth
	if inner < 1 {
		inner = 1
	}
	first := gutterPrompt
	if marker != "" {
		first = fitGutter(marker)
	}
	if ed.Empty() && placeholder != "" {
		hint := screen.Truncate(placeholder, inner-1)
		return []string{th.Render(theme.UserPrefix, first) + cursorCell(" ") + th.Render(theme.Placeholder, hint)}
	}
	rows, curRow, curCol := ed.Render(inner)
	out := make([]string, len(rows))
	for i, row := range rows {
		gutter := gutterCont
		if i == 0 {
			gutter = first
		}
		body := th.Render(theme.UserText, row)
		if i == curRow {
			body = spliceCursor(row, curCol, inner, th)
		}
		out[i] = th.Render(theme.UserPrefix, gutter) + body
	}
	return out
}

// fitGutter truncates or pads a host marker to exactly the gutter width.
func fitGutter(marker string) string {
	marker = screen.Truncate(marker, gutterWidth)
	if pad := gutterWidth - screen.Width(marker); pad > 0 {
		marker += strings.Repeat(" ", pad)
	}
	return marker
}

// spliceCursor renders the grapheme cluster at col (a space past the end of the
// row) in reverse video between the styled halves. Editor rows are plain text,
// so a direct cluster scan suffices.
//
// col is clamped below width: the editor can report a column equal to the row
// width (a line break ending a full row, or a wide cluster clipped by a narrow
// terminal), and appending a cursor cell there would overflow the row. A column
// inside a wide cluster makes the cursor cover that cluster.
func spliceCursor(row string, col, width int, th *theme.Theme) string {
	if col >= width {
		col = width - 1
	}
	var left strings.Builder
	used, rest := 0, row
	for rest != "" {
		cluster, tail, w, _ := uniseg.FirstGraphemeClusterInString(rest, -1)
		if used+w > col {
			break
		}
		left.WriteString(cluster)
		used += w
		rest = tail
	}
	cell := " "
	if rest != "" {
		cell, rest, _, _ = uniseg.FirstGraphemeClusterInString(rest, -1)
	}
	return th.Render(theme.UserText, left.String()) + cursorCell(cell) + th.Render(theme.UserText, rest)
}

// cursorCell is independent of the theme so a sparse user theme cannot hide the
// cursor.
func cursorCell(cell string) string {
	return theme.Style{Reverse: true}.Render(cell)
}
