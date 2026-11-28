// Package text holds rune-safe clipping, single-line collapsing and multi-substring
// matching, and making untrusted text inert for a terminal. Display-width truncation
// for the TUI (cell width, ANSI sequences) stays with the TUI's ansi tooling.
package text

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Clip shortens s to at most n runes, appending "..." when it cut. The cut end is
// trimmed of trailing spaces, so a clip never renders " ...". It counts runes because a
// byte slice can split a multibyte character into invalid UTF-8.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "..."
}

// OneLine collapses whitespace runs, newlines included, into single spaces and clips to
// n runes.
func OneLine(s string, n int) string {
	return Clip(strings.Join(strings.Fields(s), " "), n)
}

// ContainsAny reports whether s contains any of the substrings. It is case-sensitive.
func ContainsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Inert removes the runes a terminal acts on rather than prints: C0 controls, DEL and
// the C1 range, so text a model or a tool produced cannot move the cursor, retitle the
// window or open an escape sequence when it is written to the screen. What is left of
// an escape sequence ("[31m") prints as ordinary characters. A newline is kept, since
// the caller decides how lines break. A tab becomes the spaces that reach the next
// [TabStop] column, counted in display cells after the controls are gone, so indented
// shell output and tab-aligned tables keep their columns.
func Inert(s string) string {
	if !strings.ContainsFunc(s, acts) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	col := 0
	g := uniseg.NewGraphemes(strings.Map(func(r rune) rune {
		if r != '\t' && acts(r) {
			return -1
		}
		return r
	}, s))
	for g.Next() {
		switch c := g.Str(); c {
		case "\t":
			n := TabStop - col%TabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case "\n":
			b.WriteByte('\n')
			col = 0
		default:
			b.WriteString(c)
			col += g.Width()
		}
	}
	return b.String()
}

// TabStop is the column interval [Inert] expands a tab to, the terminal default.
const TabStop = 8

func acts(r rune) bool {
	return (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f)
}
