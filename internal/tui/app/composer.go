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

// composerRows renders the prompt editor as styled rows. The hardware cursor
// is hidden for the session (the painter rests it at the start of the last
// live row, not at the edit point), so the cursor the user sees is a
// reverse-video cell spliced into the row at the editor's reported position.
func composerRows(ed *editor.Editor, th *theme.Theme, width int, placeholder string) []string {
	inner := width - gutterWidth
	if inner < 1 {
		inner = 1
	}
	if ed.Empty() && placeholder != "" {
		hint := screen.Truncate(placeholder, inner-1)
		return []string{th.Render(theme.UserPrefix, gutterPrompt) + cursorCell(" ") + th.Render(theme.Placeholder, hint)}
	}
	rows, curRow, curCol := ed.Render(inner)
	out := make([]string, len(rows))
	for i, row := range rows {
		gutter := gutterCont
		if i == 0 {
			gutter = gutterPrompt
		}
		body := th.Render(theme.UserText, row)
		if i == curRow {
			body = spliceCursor(row, curCol, inner, th)
		}
		out[i] = th.Render(theme.UserPrefix, gutter) + body
	}
	return out
}

// spliceCursor styles row with the cursor over the cell at col. Editor rows
// are plain text with no escape sequences, so the cell walk is a direct
// grapheme-cluster scan; the cluster under the cursor (or a space when the
// cursor rests past the end of the row) renders in reverse video between the
// normally styled halves.
//
// col is clamped below width so the spliced row never exceeds it: the editor
// can report a cursor column equal to the row width (the cursor on a line
// break ending a full row, or on a wide cluster clipped by a very narrow
// terminal), and appending a cursor cell there would overflow the row the
// painter was promised. The walk never consumes a cluster that would cross
// the clamped column: when the column lands inside a wide cluster, the
// cursor covers that cluster, keeping the row within width instead of
// pushing the appended cell past it.
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

// cursorCell renders the cell under the cursor in plain reverse video,
// independent of the theme, so the cursor never vanishes under a sparse user
// theme file.
func cursorCell(cell string) string {
	return theme.Style{Reverse: true}.Render(cell)
}
