package render

import (
	"strings"

	east "github.com/yuin/goldmark/extension/ast"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// table lays out a GFM table: column widths from content, shrunk widest-first
// when the terminal is narrower than the natural layout, cells truncated with
// a ".." marker rather than wrapped so rows stay scannable.
func (m *Markdown) table(t *east.Table, src []byte, width int, base theme.Style) []string {
	var (
		header []([]span)
		rows   [][]([]span)
		aligns []east.Alignment
	)
	for r := t.FirstChild(); r != nil; r = r.NextSibling() {
		var cells []([]span)
		cellBase := base
		if _, isHeader := r.(*east.TableHeader); isHeader {
			cellBase = m.style(theme.Strong, base)
		}
		for c := r.FirstChild(); c != nil; c = c.NextSibling() {
			cell, ok := c.(*east.TableCell)
			if !ok {
				continue
			}
			cells = append(cells, m.inline(cell, src, cellBase))
			if _, isHeader := r.(*east.TableHeader); isHeader {
				aligns = append(aligns, cell.Alignment)
			}
		}
		if _, isHeader := r.(*east.TableHeader); isHeader {
			header = cells
		} else {
			rows = append(rows, cells)
		}
	}

	cols := len(header)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return nil
	}

	widths := make([]int, cols)
	measure := func(cells []([]span)) {
		for i, c := range cells {
			if w := textWidth(plainText(c)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(header)
	for _, r := range rows {
		measure(r)
	}
	for i := range widths {
		if widths[i] < 1 {
			widths[i] = 1
		}
	}
	fitColumns(widths, width-3*(cols-1))

	border := m.style(theme.Border, theme.Style{})
	renderRow := func(cells []([]span)) string {
		parts := make([]string, cols)
		for i := range cols {
			var cell []span
			if i < len(cells) {
				cell = cells[i]
			}
			cell = truncateSpans(cell, widths[i], "..")
			cell = alignSpans(cell, widths[i], alignAt(aligns, i))
			parts[i] = lineOf(padSpans(cell, widths[i]))
		}
		return strings.Join(parts, border.Render(" │ "))
	}

	var out []string
	if len(header) > 0 {
		out = append(out, renderRow(header))
		seps := make([]string, cols)
		for i, w := range widths {
			seps[i] = strings.Repeat("─", w)
		}
		out = append(out, border.Render(strings.Join(seps, "─┼─")))
	}
	for _, r := range rows {
		out = append(out, renderRow(r))
	}
	return out
}

// fitColumns shrinks the widest columns first until the total fits the
// budget, never below a floor of 3 cells. A terminal too narrow even for the
// floors keeps the floors: the painter's overflow guard truncates the rest,
// which is the only honest option left.
func fitColumns(widths []int, budget int) {
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > budget {
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= 3 {
			return
		}
		widths[widest]--
		total--
	}
}

// alignAt returns the header-declared alignment for a column, defaulting to
// left for columns past the header.
func alignAt(aligns []east.Alignment, i int) east.Alignment {
	if i < len(aligns) {
		return aligns[i]
	}
	return east.AlignNone
}

// alignSpans pads a cell on the correct side(s) for its alignment. Left and
// none need no leading pad; padSpans finishes the right side either way.
func alignSpans(cell []span, width int, a east.Alignment) []span {
	used := textWidth(plainText(cell))
	if used >= width {
		return cell
	}
	gap := width - used
	switch a {
	case east.AlignRight:
		return append([]span{{strings.Repeat(" ", gap), theme.Style{}}}, cell...)
	case east.AlignCenter:
		left := gap / 2
		if left == 0 {
			return cell
		}
		return append([]span{{strings.Repeat(" ", left), theme.Style{}}}, cell...)
	default:
		return cell
	}
}
