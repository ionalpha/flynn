// Package render turns session content into pre-styled, pre-wrapped terminal
// lines: markdown through a goldmark AST walk, fenced code through a chroma
// highlighter bucketed into the theme's syntax roles, and file changes
// through a width-adaptive diff view. Everything it returns obeys the screen
// contract: each string is one terminal row no wider than the given width,
// and every styled span is self-contained (prefix, text, reset), so the
// output can be committed to scrollback or repainted in the live region
// without any escape-state bookkeeping.
//
// The package is pure: no clock, no terminal, nothing mutated per render.
// The same source at the same width always renders to the same lines, which
// keeps it property-testable and safe to call from a replayed session.
package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// span is a run of plain text under one effective style. All rendering in
// this package flows through spans: walkers merge nested roles into one
// Style per run, and only the final line assembly emits escape sequences.
type span struct {
	text  string
	style theme.Style
}

// lineOf assembles one terminal row from spans, merging adjacent spans that
// share a style so a heavily nested line does not drown in escape sequences.
// Control bytes are stripped as a final guard: spans carry plain text only,
// and one stray carriage return from source material would break the
// one-string-one-row contract every consumer relies on.
func lineOf(spans []span) string {
	var b strings.Builder
	for i := 0; i < len(spans); {
		var text strings.Builder
		text.WriteString(spans[i].text)
		j := i + 1
		for j < len(spans) && spans[j].style == spans[i].style {
			text.WriteString(spans[j].text)
			j++
		}
		b.WriteString(spans[i].style.Render(controlFree(text.String())))
		i = j
	}
	return b.String()
}

// controlFree drops C0 control bytes and DEL from plain text. Zero-width in
// the width accounting already, so stripping them changes no layout, only
// removes their side effects on a real terminal.
func controlFree(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// clampRows is the hard width-overflow guard: any row still wider than the
// width (a grapheme cluster wider than a pathological width, a table whose
// column floors do not fit) is truncated, with a reset appended so a style
// cut mid-span cannot bleed into the next row.
func clampRows(rows []string, width int) []string {
	for i, r := range rows {
		if ansi.StringWidth(r) > width {
			rows[i] = ansi.Truncate(r, width, "") + "\x1b[0m"
		}
	}
	return rows
}

// textWidth is the display width of plain (escape-free) text in terminal
// cells, grapheme-cluster correct.
func textWidth(s string) int { return ansi.StringWidth(s) }

// token is one unit of line filling: a word (a maximal run of non-space text,
// possibly crossing style boundaries), or a forced line break.
type token struct {
	word  []span
	width int
	brk   bool
}

// tokenize splits spans into words and explicit breaks. Spaces separate
// words and are re-inserted by the filler, so a space that lands on a wrap
// point disappears, matching ordinary text flow.
func tokenize(spans []span) []token {
	var (
		toks []token
		word []span
		ww   int
	)
	endWord := func() {
		if len(word) > 0 {
			toks = append(toks, token{word: word, width: ww})
			word, ww = nil, 0
		}
	}
	for _, s := range spans {
		rest := s.text
		for rest != "" {
			cut := strings.IndexAny(rest, " \t\n")
			if cut < 0 {
				word = append(word, span{rest, s.style})
				ww += textWidth(rest)
				break
			}
			if cut > 0 {
				word = append(word, span{rest[:cut], s.style})
				ww += textWidth(rest[:cut])
			}
			endWord()
			if rest[cut] == '\n' {
				toks = append(toks, token{brk: true})
			}
			rest = rest[cut+1:]
		}
	}
	endWord()
	return toks
}

// wrapSpans word-wraps styled spans to the width and assembles the resulting
// rows. Breaks happen at spaces and at newlines inside a span; a word wider
// than a whole line is hard-broken at grapheme boundaries so no row can ever
// exceed the width.
func wrapSpans(spans []span, width int) []string {
	if width < 1 {
		return nil
	}
	var (
		out  []string
		line []span
		used int
	)
	flush := func() {
		out = append(out, lineOf(line))
		line, used = nil, 0
	}
	for _, t := range tokenize(spans) {
		switch {
		case t.brk:
			flush()
		case t.width <= width:
			gap := 0
			if used > 0 {
				gap = 1
			}
			if used+gap+t.width > width {
				flush()
				gap = 0
			}
			if gap == 1 {
				line = append(line, span{" ", theme.Style{}})
				used++
			}
			line = append(line, t.word...)
			used += t.width
		default:
			// An overlong word starts on its own row and hard-breaks.
			if used > 0 {
				flush()
			}
			for _, w := range t.word {
				rest := w.text
				for rest != "" {
					g, tail, gw := nextGrapheme(rest)
					if used+gw > width && used > 0 {
						flush()
					}
					line = append(line, span{g, w.style})
					used += gw
					rest = tail
				}
			}
		}
	}
	if len(line) > 0 {
		flush()
	}
	return out
}

// nextGrapheme cuts the first grapheme cluster off s and reports its cell
// width, so hard breaks never split an emoji or a combining sequence.
func nextGrapheme(s string) (cluster, rest string, width int) {
	cluster, rest, w, _ := uniseg.FirstGraphemeClusterInString(s, -1)
	return cluster, rest, w
}

// hardWrapLine breaks one pre-styled logical line into rows of at most
// width cells without ever dropping content, preserving each span's style.
// It is the wrap used where spaces are content, not separators: code lines
// and diff lines.
func hardWrapLine(spans []span, width int) []string {
	if width < 1 {
		return nil
	}
	var (
		out  []string
		line []span
		used int
	)
	for _, s := range spans {
		rest := s.text
		for rest != "" {
			g, tail, gw := nextGrapheme(rest)
			if used+gw > width && used > 0 {
				out = append(out, lineOf(line))
				line, used = nil, 0
			}
			line = append(line, span{g, s.style})
			used += gw
			rest = tail
		}
	}
	out = append(out, lineOf(line))
	return out
}

// truncateSpans cuts styled spans to at most width cells, appending the tail
// marker when anything was cut. Used where a row must stay single-line, such
// as table cells and split-diff columns.
func truncateSpans(spans []span, width int, tail string) []span {
	total := 0
	for _, s := range spans {
		total += textWidth(s.text)
	}
	if total <= width {
		return spans
	}
	tw := textWidth(tail)
	budget := width - tw
	if budget < 0 {
		budget = 0
	}
	var out []span
	used, full := 0, false
	for _, s := range spans {
		rest := s.text
		var kept strings.Builder
		for rest != "" {
			g, next, gw := nextGrapheme(rest)
			if used+gw > budget {
				full = true
				break
			}
			kept.WriteString(g)
			used += gw
			rest = next
		}
		if kept.Len() > 0 {
			out = append(out, span{kept.String(), s.style})
		}
		if full {
			break
		}
	}
	if tail != "" {
		out = append(out, span{tail, theme.Style{}})
	}
	return out
}

// padSpans right-pads spans with plain spaces to exactly width cells.
func padSpans(spans []span, width int) []span {
	used := 0
	for _, s := range spans {
		used += textWidth(s.text)
	}
	if used >= width {
		return spans
	}
	return append(append([]span{}, spans...), span{strings.Repeat(" ", width-used), theme.Style{}})
}

// prefixLines prepends a styled prefix to every line, used for blockquotes
// and list continuations. The prefix is rendered once per line, so each row
// stays self-contained.
func prefixLines(lines []string, first, rest span) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		out[i] = p.style.Render(p.text) + l
	}
	return out
}
