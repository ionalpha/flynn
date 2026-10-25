package render

import (
	"strconv"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// splitDiffMinWidth is the narrowest terminal that renders diffs
// side-by-side; below it the unified layout reads better.
const splitDiffMinWidth = 100

// Diff renders the change from before to after as themed terminal lines:
// a header naming the path with added and removed counts, then hunks with
// line-number gutters. The layout adapts to width: side-by-side columns on a
// wide terminal, unified below that. Paired changed lines carry a word-level
// highlight (reverse video over the add and remove styles) so a one-token
// edit inside a long line is findable at a glance.
func Diff(th *theme.Theme, path, before, after string, width int) []string {
	if width < 1 {
		return nil
	}
	// Normalized before diffing so wrap accounting and output agree on every
	// byte (see Markdown.Render); diffing the normalized text also keeps the
	// intraline pairing consistent with what is shown.
	before = strings.ToValidUTF8(before, "�")
	after = strings.ToValidUTF8(after, "�")
	edits := udiff.Strings(before, after)
	u, err := udiff.ToUnifiedDiff(path, path, before, edits, udiff.DefaultContextLines)
	if err != nil || len(u.Hunks) == 0 {
		return nil
	}

	adds, dels := 0, 0
	for _, h := range u.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case udiff.Insert:
				adds++
			case udiff.Delete:
				dels++
			case udiff.Equal:
			}
		}
	}
	header := []span{
		{path, th.Style(theme.DiffLocation)},
		{" ", theme.Style{}},
		{"+" + strconv.Itoa(adds), th.Style(theme.DiffAdded)},
		{" ", theme.Style{}},
		{"-" + strconv.Itoa(dels), th.Style(theme.DiffRemoved)},
	}
	out := hardWrapLine(header, width)

	if width >= splitDiffMinWidth {
		return clampRows(append(out, splitHunks(th, u, width)...), width)
	}
	return clampRows(append(out, unifiedHunks(th, u, width)...), width)
}

// hunkRow is one logical diff row: pre-pairing has already decided which old
// and new line (if either) it shows.
type hunkRow struct {
	kind    udiff.OpKind
	oldN    int // 1-based old line number, 0 when absent
	newN    int
	content []span
}

// pairHunk walks a hunk's lines and produces rows with line numbers, pairing
// each run of deletes with the following run of inserts. Equal-length runs
// get the word-level intraline highlight.
func pairHunk(th *theme.Theme, h *udiff.Hunk) []hunkRow {
	var rows []hunkRow
	oldN, newN := h.FromLine, h.ToLine
	lines := h.Lines
	for i := 0; i < len(lines); {
		switch lines[i].Kind {
		case udiff.Equal:
			rows = append(rows, hunkRow{
				kind: udiff.Equal, oldN: oldN, newN: newN,
				content: []span{{clean(lines[i].Content), th.Style(theme.DiffContext)}},
			})
			oldN++
			newN++
			i++
		default:
			delStart := i
			for i < len(lines) && lines[i].Kind == udiff.Delete {
				i++
			}
			insStart := i
			for i < len(lines) && lines[i].Kind == udiff.Insert {
				i++
			}
			dels, inss := lines[delStart:insStart], lines[insStart:i]
			paired := len(dels) == len(inss)
			for j, l := range dels {
				content := []span{{clean(l.Content), th.Style(theme.DiffRemoved)}}
				if paired {
					content = intraline(clean(l.Content), clean(inss[j].Content), th.Style(theme.DiffRemoved))
				}
				rows = append(rows, hunkRow{kind: udiff.Delete, oldN: oldN, content: content})
				oldN++
			}
			for j, l := range inss {
				content := []span{{clean(l.Content), th.Style(theme.DiffAdded)}}
				if paired {
					content = intraline(clean(l.Content), clean(dels[j].Content), th.Style(theme.DiffAdded))
				}
				rows = append(rows, hunkRow{kind: udiff.Insert, newN: newN, content: content})
				newN++
			}
		}
	}
	return rows
}

// intraline styles line against other: the shared prefix and suffix keep the
// base style, the differing middle adds reverse video. Computed on runes, so
// it is a character-range highlight anchored at word-edit granularity in the
// common case of one changed token.
func intraline(line, other string, base theme.Style) []span {
	a, b := []rune(line), []rune(other)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	if p == 0 && s == 0 {
		return []span{{line, base}}
	}
	hot := (theme.Style{Reverse: true}).Over(base)
	var out []span
	if p > 0 {
		out = append(out, span{string(a[:p]), base})
	}
	if mid := string(a[p : len(a)-s]); mid != "" {
		out = append(out, span{mid, hot})
	}
	if s > 0 {
		out = append(out, span{string(a[len(a)-s:]), base})
	}
	return out
}

// unifiedHunks renders the classic single-column layout with an old and new
// line-number gutter. Content rows hard-wrap; continuation rows keep a blank
// gutter so line numbers stay truthful.
func unifiedHunks(th *theme.Theme, u udiff.UnifiedDiff, width int) []string {
	gw := gutterWidth(u)
	// gutter + gutter + sign, each with one trailing space.
	prefix := 2*(gw+1) + 2
	if width <= prefix {
		return nil
	}
	var out []string
	muted := th.Style(theme.Muted)
	for _, h := range u.Hunks {
		out = append(out, hardWrapLine([]span{{hunkLabel(h), th.Style(theme.DiffLocation)}}, width)...)
		for _, r := range pairHunk(th, h) {
			sign, signStyle := " ", th.Style(theme.DiffContext)
			switch r.kind {
			case udiff.Delete:
				sign, signStyle = "-", th.Style(theme.DiffRemoved)
			case udiff.Insert:
				sign, signStyle = "+", th.Style(theme.DiffAdded)
			case udiff.Equal:
			}
			gutter := lineOf([]span{
				{num(r.oldN, gw) + " " + num(r.newN, gw) + " ", muted},
				{sign + " ", signStyle},
			})
			for i, body := range hardWrapLine(r.content, width-prefix) {
				if i == 0 {
					out = append(out, gutter+body)
				} else {
					out = append(out, strings.Repeat(" ", prefix)+body)
				}
			}
		}
	}
	return out
}

// splitHunks renders the side-by-side layout: old on the left, new on the
// right, each with its own gutter, single-row cells truncated rather than
// wrapped so the two sides stay row-aligned.
func splitHunks(th *theme.Theme, u udiff.UnifiedDiff, width int) []string {
	gw := gutterWidth(u)
	col := (width - 3) / 2
	cell := col - gw - 3 // gutter, space, sign, space
	if cell < 1 {
		return unifiedHunks(th, u, width)
	}
	v := splitView{th: th, gw: gw, col: col, cell: cell}
	var out []string
	for _, h := range u.Hunks {
		out = append(out, hardWrapLine([]span{{hunkLabel(h), th.Style(theme.DiffLocation)}}, width)...)
		out = append(out, v.hunk(h)...)
	}
	return out
}

// splitView is the fixed geometry of a side-by-side diff: the gutter width,
// the column width, and the cell width left for content after the gutter and
// sign.
type splitView struct {
	th            *theme.Theme
	gw, col, cell int
}

// side renders one column: line number, sign, then content truncated to the
// cell and padded to the column.
func (v splitView) side(n int, sign string, signStyle theme.Style, content []span) string {
	spans := []span{{num(n, v.gw) + " ", v.th.Style(theme.Muted)}, {sign + " ", signStyle}}
	spans = append(spans, truncateSpans(content, v.cell, "..")...)
	return lineOf(padSpans(spans, v.col))
}

// blank is the empty column opposite an unpaired line.
func (v splitView) blank() string { return lineOf(padSpans(nil, v.col)) }

func (v splitView) join(left, right string) string {
	return left + v.th.Style(theme.Border).Render(" │ ") + right
}

// hunk renders one hunk's rows, numbering the old and new sides as it walks.
func (v splitView) hunk(h *udiff.Hunk) []string {
	w := hunkWalker{v: v, oldN: h.FromLine, newN: h.ToLine}
	lines := make([]diffLine, len(h.Lines))
	for i, l := range h.Lines {
		lines[i] = diffLine{kind: l.Kind, text: clean(l.Content)}
	}
	for i := 0; i < len(lines); {
		if lines[i].kind == udiff.Equal {
			w.context(lines[i])
			i++
			continue
		}
		delEnd := runEnd(lines, i, udiff.Delete)
		insEnd := runEnd(lines, delEnd, udiff.Insert)
		w.change(lines[i:delEnd], lines[delEnd:insEnd])
		i = insEnd
	}
	return w.out
}

// diffLine is a hunk line with its text already cleaned for display;
// go-udiff does not export its line type.
type diffLine struct {
	kind udiff.OpKind
	text string
}

// runEnd is the index just past the run of kind lines starting at i.
func runEnd(lines []diffLine, i int, kind udiff.OpKind) int {
	for i < len(lines) && lines[i].kind == kind {
		i++
	}
	return i
}

// hunkWalker accumulates a hunk's rows and the next old and new line numbers.
type hunkWalker struct {
	v          splitView
	oldN, newN int
	out        []string
}

// context emits an unchanged line on both sides.
func (w *hunkWalker) context(l diffLine) {
	style := w.v.th.Style(theme.DiffContext)
	content := []span{{l.text, style}}
	w.out = append(w.out, w.v.join(
		w.v.side(w.oldN, " ", style, content),
		w.v.side(w.newN, " ", style, content),
	))
	w.oldN++
	w.newN++
}

// change emits a run of deletes against the run of inserts that follows it,
// row by row. Equal-length runs get the word-level highlight on each pair.
func (w *hunkWalker) change(dels, inss []diffLine) {
	paired := len(dels) == len(inss)
	for j := 0; j < len(dels) || j < len(inss); j++ {
		left, right := w.v.blank(), w.v.blank()
		if j < len(dels) {
			left = w.removed(dels[j], inss, j, paired)
		}
		if j < len(inss) {
			right = w.added(inss[j], dels, j, paired)
		}
		w.out = append(w.out, w.v.join(left, right))
	}
}

func (w *hunkWalker) removed(l diffLine, inss []diffLine, j int, paired bool) string {
	style := w.v.th.Style(theme.DiffRemoved)
	content := []span{{l.text, style}}
	if paired {
		content = intraline(l.text, inss[j].text, style)
	}
	row := w.v.side(w.oldN, "-", style, content)
	w.oldN++
	return row
}

func (w *hunkWalker) added(l diffLine, dels []diffLine, j int, paired bool) string {
	style := w.v.th.Style(theme.DiffAdded)
	content := []span{{l.text, style}}
	if paired {
		content = intraline(l.text, dels[j].text, style)
	}
	row := w.v.side(w.newN, "+", style, content)
	w.newN++
	return row
}

// hunkLabel is the location line above a hunk.
func hunkLabel(h *udiff.Hunk) string {
	return "@@ -" + strconv.Itoa(h.FromLine) + " +" + strconv.Itoa(h.ToLine) + " @@"
}

// gutterWidth is the digit width needed for the diff's largest line number.
func gutterWidth(u udiff.UnifiedDiff) int {
	largest := 1
	for _, h := range u.Hunks {
		n := h.FromLine
		if h.ToLine > n {
			n = h.ToLine
		}
		n += len(h.Lines)
		if n > largest {
			largest = n
		}
	}
	return len(strconv.Itoa(largest))
}

// num formats a line number right-aligned to the gutter, blank when absent.
func num(n, gw int) string {
	if n == 0 {
		return strings.Repeat(" ", gw)
	}
	s := strconv.Itoa(n)
	return strings.Repeat(" ", gw-len(s)) + s
}

// clean strips the trailing newline a unified-diff line carries and expands
// tabs to four cells so gutters stay aligned.
func clean(s string) string {
	s = strings.TrimRight(s, "\n")
	s = strings.TrimRight(s, "\r")
	return strings.ReplaceAll(s, "\t", "    ")
}
