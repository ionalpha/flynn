// Package text holds the small string helpers that were re-implemented across
// the tree: rune-safe clipping (one of the copies sliced bytes and could split
// a multibyte character), single-line collapsing, and multi-substring matching.
// Classification predicates and display truncation both hang on these, so they
// live once. Display-width truncation for the TUI is a different concern (cell
// width, ANSI sequences) and stays with the TUI's ansi tooling.
package text

import "strings"

// Clip shortens s to at most n runes, appending "..." when it cut. The cut end
// is trimmed of trailing spaces so a clip never renders " ...". Rune-based on
// purpose: a byte slice can split a multibyte character and produce invalid
// UTF-8.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "..."
}

// OneLine collapses whitespace runs (including newlines) into single spaces and
// clips to n runes, so a multi-line tool output or model message renders as a
// single tidy line.
func OneLine(s string, n int) string {
	return Clip(strings.Join(strings.Fields(s), " "), n)
}

// ContainsAny reports whether s contains any of the substrings. Callers
// normalize case themselves when they need to.
func ContainsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
