// Package render turns session content into pre-styled, pre-wrapped terminal
// lines: markdown through a goldmark AST walk, fenced code through chroma
// bucketed into the theme's syntax roles, and file changes through a
// width-adaptive diff view. Each returned string is one row no wider than the
// given width, and every styled span is self-contained (prefix, text, reset), so
// output can go to scrollback or the live region with no escape-state
// bookkeeping.
//
// The package is pure: no clock, no terminal, nothing mutated per render. The
// same source at the same width renders to the same lines, so it is
// property-testable and safe in a replayed session.
package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/ionalpha/flynn/internal/tui/theme"
)

// span is plain text under one effective style. Walkers merge nested roles into
// one Style per run, and only lineOf emits escape sequences.
type span struct {
	text  string
	style theme.Style
}

// lineOf assembles one row from spans, merging adjacent spans of one style.
// Control bytes are stripped as a final guard: a stray carriage return from
// source material would break the one-string-one-row contract.
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

// controlFree drops C0 control bytes, DEL and the C1 controls, among them the
// one-byte form of the escape sequence introducer. They are zero-width, so layout
// is unchanged; only their terminal side effects go.
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

func isControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }

// clampRows truncates any row still wider than width (a cluster wider than a
// pathological width, a table whose column floors do not fit), appending a reset
// so a style cut mid-span cannot bleed into the next row.
//
// The cut is tightened until [ansi.StringWidth], the measure callers lay rows out
// by, agrees it fits, because [ansi.Truncate] measures some clusters narrower (a
// space followed by U+FE0F is one cell to Truncate and two to StringWidth).
func clampRows(rows []string, width int) []string {
	for i, r := range rows {
		for cut := width; ansi.StringWidth(rows[i]) > width && cut >= 0; cut-- {
			rows[i] = ansi.Truncate(r, cut, "") + "\x1b[0m"
		}
	}
	return rows
}

func textWidth(s string) int { return ansi.StringWidth(s) }

// token is a word (a maximal run of non-space text, possibly crossing styles) or
// a forced line break.
type token struct {
	word  []span
	width int
	brk   bool
}

// tokenize splits spans into words and breaks. The filler re-inserts spaces, so
// one landing on a wrap point disappears.
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

// wrapSpans word-wraps spans at spaces and newlines. A word wider than a line is
// hard-broken at grapheme boundaries so no row exceeds width.
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

// nextGrapheme cuts the first grapheme cluster off s, so hard breaks never split
// an emoji or combining sequence.
func nextGrapheme(s string) (cluster, rest string, width int) {
	cluster, rest, w, _ := uniseg.FirstGraphemeClusterInString(s, -1)
	return cluster, rest, w
}

// hardWrapLine breaks one line into rows of at most width cells, dropping
// nothing. It wraps where spaces are content: code and diff lines.
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

// truncateSpans cuts spans to width cells, appending tail when anything was cut,
// for rows that must stay single-line (table cells, split-diff columns).
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

// prefixLines prepends first to the first line and rest to the others, rendering
// each separately so every row stays self-contained.
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
