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
	border := th.Style(theme.Border)
	muted := th.Style(theme.Muted)
	side := func(n int, sign string, signStyle theme.Style, content []span) string {
		spans := []span{{num(n, gw) + " ", muted}, {sign + " ", signStyle}}
		spans = append(spans, truncateSpans(content, cell, "..")...)
		return lineOf(padSpans(spans, col))
	}
	blank := func() string { return lineOf(padSpans(nil, col)) }

	join := func(left, right string) string { return left + border.Render(" │ ") + right }

	var out []string
	for _, h := range u.Hunks {
		out = append(out, hardWrapLine([]span{{hunkLabel(h), th.Style(theme.DiffLocation)}}, width)...)
		oldN, newN := h.FromLine, h.ToLine
		lines := h.Lines
		for i := 0; i < len(lines); {
			if lines[i].Kind == udiff.Equal {
				content := []span{{clean(lines[i].Content), th.Style(theme.DiffContext)}}
				out = append(out, join(
					side(oldN, " ", th.Style(theme.DiffContext), content),
					side(newN, " ", th.Style(theme.DiffContext), content),
				))
				oldN++
				newN++
				i++
				continue
			}
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
			for j := 0; j < len(dels) || j < len(inss); j++ {
				left, right := blank(), blank()
				if j < len(dels) {
					content := []span{{clean(dels[j].Content), th.Style(theme.DiffRemoved)}}
					if paired {
						content = intraline(clean(dels[j].Content), clean(inss[j].Content), th.Style(theme.DiffRemoved))
					}
					left = side(oldN, "-", th.Style(theme.DiffRemoved), content)
					oldN++
				}
				if j < len(inss) {
					content := []span{{clean(inss[j].Content), th.Style(theme.DiffAdded)}}
					if paired {
						content = intraline(clean(inss[j].Content), clean(dels[j].Content), th.Style(theme.DiffAdded))
					}
					right = side(newN, "+", th.Style(theme.DiffAdded), content)
					newN++
				}
				out = append(out, join(left, right))
			}
		}
	}
	return out
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
