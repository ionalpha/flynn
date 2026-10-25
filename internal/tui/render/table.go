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
	g := m.tableCells(t, src, base)
	cols := g.columns()
	if cols == 0 {
		return nil
	}
	widths := g.widths(cols)
	fitColumns(widths, width-3*(cols-1))

	border := m.style(theme.Border, theme.Style{})
	var out []string
	if len(g.header) > 0 {
		out = append(out, g.renderRow(g.header, widths, border))
		seps := make([]string, cols)
		for i, w := range widths {
			seps[i] = strings.Repeat("─", w)
		}
		out = append(out, border.Render(strings.Join(seps, "─┼─")))
	}
	for _, r := range g.rows {
		out = append(out, g.renderRow(r, widths, border))
	}
	return out
}

// tableGrid is a table's inline-rendered cells, split into the header row
// and the body, with the alignment each header cell declares.
type tableGrid struct {
	header []([]span)
	rows   [][]([]span)
	aligns []east.Alignment
}

// tableCells renders every cell's inline content. Header cells are bold, and
// their alignments govern the columns below them.
func (m *Markdown) tableCells(t *east.Table, src []byte, base theme.Style) tableGrid {
	var g tableGrid
	for r := t.FirstChild(); r != nil; r = r.NextSibling() {
		_, isHeader := r.(*east.TableHeader)
		cellBase := base
		if isHeader {
			cellBase = m.style(theme.Strong, base)
		}
		var cells []([]span)
		for c := r.FirstChild(); c != nil; c = c.NextSibling() {
			cell, ok := c.(*east.TableCell)
			if !ok {
				continue
			}
			cells = append(cells, m.inline(cell, src, cellBase))
			if isHeader {
				g.aligns = append(g.aligns, cell.Alignment)
			}
		}
		if isHeader {
			g.header = cells
		} else {
			g.rows = append(g.rows, cells)
		}
	}
	return g
}

// columns is the widest row's cell count.
func (g tableGrid) columns() int {
	cols := len(g.header)
	for _, r := range g.rows {
		cols = max(cols, len(r))
	}
	return cols
}

// widths is each column's natural width: its widest cell, at least one.
func (g tableGrid) widths(cols int) []int {
	widths := make([]int, cols)
	for i := range widths {
		widths[i] = 1
	}
	measure := func(cells []([]span)) {
		for i, c := range cells {
			widths[i] = max(widths[i], textWidth(plainText(c)))
		}
	}
	measure(g.header)
	for _, r := range g.rows {
		measure(r)
	}
	return widths
}

// renderRow truncates, aligns and pads each cell to its column. A row shorter
// than the table gets empty cells.
func (g tableGrid) renderRow(cells []([]span), widths []int, border theme.Style) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		var cell []span
		if i < len(cells) {
			cell = cells[i]
		}
		cell = truncateSpans(cell, w, "..")
		cell = alignSpans(cell, w, alignAt(g.aligns, i))
		parts[i] = lineOf(padSpans(cell, w))
	}
	return strings.Join(parts, border.Render(" │ "))
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
