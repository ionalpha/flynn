// Package fuzzy ranks completion candidates against a typed pattern. The
// matcher is the subsequence kind every editor's file picker uses: the
// pattern's characters must appear in the candidate in order, and the score
// prefers matches at path-segment and word starts, consecutive runs, and
// short candidates, so "sre" finds "screen/render.go" ahead of an accidental
// scatter of the same letters. Matching is case-insensitive; an exact-case
// hit earns a small bonus.
//
// Everything here is pure and deterministic: same inputs, same order, no
// clock, no randomness. Ties in Rank break by shorter candidate, then
// lexically, so the menu never reshuffles between keystrokes.
package fuzzy

import (
	"sort"
	"unicode"
)

// Scoring weights. The exact values matter less than their order: a segment
// start outranks a camelCase hump, which outranks mere adjacency, and every
// bonus outranks the per-gap penalty, so structure wins over proximity.
const (
	bonusSegmentStart = 8 // match right after a separator, or at the start
	bonusCamel        = 6 // match on an upper rune following a lower rune
	bonusConsecutive  = 5 // match adjacent to the previous match
	bonusExactCase    = 1 // pattern rune matches without case folding
	penaltyGap        = 1 // per candidate rune skipped between matches
)

// Score reports how well pattern matches candidate, and whether it matches
// at all. The empty pattern matches everything with score zero. A greedy
// forward scan keeps the cost linear in the candidate; it can under-score an
// adversarial alignment, but it never misses a match.
func Score(pattern, candidate string) (int, bool) {
	if pattern == "" {
		return 0, true
	}
	ps := []rune(pattern)
	cs := []rune(candidate)
	score, pi, last := 0, 0, -1
	var prev rune
	for ci, r := range cs {
		if pi < len(ps) && unicode.ToLower(r) == unicode.ToLower(ps[pi]) {
			switch {
			case ci == 0 || isSeparator(prev):
				score += bonusSegmentStart
			case unicode.IsUpper(r) && unicode.IsLower(prev):
				score += bonusCamel
			case last == ci-1:
				score += bonusConsecutive
			}
			if r == ps[pi] {
				score += bonusExactCase
			}
			if last >= 0 && ci-last > 1 {
				score -= penaltyGap * (ci - last - 1)
			}
			last = ci
			pi++
		}
		prev = r
	}
	if pi < len(ps) {
		return 0, false
	}
	return score, true
}

// isSeparator reports whether r starts a new segment for scoring purposes:
// path separators, the word punctuation common in file names, and spaces.
func isSeparator(r rune) bool {
	switch r {
	case '/', '\\', '-', '_', '.', ' ':
		return true
	}
	return false
}

// Rank returns the best matches for pattern among candidates, best first,
// at most limit long. An optional bonus hook adds candidate-specific score
// (recent picks, pinned entries); nil adds nothing. Order is deterministic:
// score descending, then shorter candidate, then lexical.
func Rank(pattern string, candidates []string, limit int, bonus func(string) int) []string {
	if limit <= 0 {
		return nil
	}
	type match struct {
		text  string
		score int
	}
	matches := make([]match, 0, len(candidates))
	for _, c := range candidates {
		s, ok := Score(pattern, c)
		if !ok {
			continue
		}
		if bonus != nil {
			s += bonus(c)
		}
		matches = append(matches, match{text: c, score: s})
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if len(a.text) != len(b.text) {
			return len(a.text) < len(b.text)
		}
		return a.text < b.text
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.text
	}
	return out
}
