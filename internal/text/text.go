// Package text holds rune-safe clipping, single-line collapsing and multi-substring
// matching. Display-width truncation for the TUI (cell width, ANSI sequences) stays
// with the TUI's ansi tooling.
package text

import "strings"

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
