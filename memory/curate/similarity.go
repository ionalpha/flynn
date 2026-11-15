package curate

import "strings"

// minAbbreviation is the shortest abbreviation counted, because one letter would
// match every subject sharing it.
const minAbbreviation = 2

// SubjectSimilarity scores two normalized subjects in [0,1], 1 identical. It compares
// tokens, because a whole-string character measure scores "db-choice" closer to
// "queue-choice" than to "database-choice". An abbreviation ("db" for "database")
// is a partial match. The pairing runs both ways and the lower score wins, so it is
// symmetric and errs against firing. It cannot see synonyms.
func SubjectSimilarity(a, b string) float64 {
	if a == b {
		if a == "" {
			return 0
		}
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	ta, tb := strings.Split(a, "-"), strings.Split(b, "-")
	return min(greedyScore(ta, tb), greedyScore(tb, ta))
}

// greedyScore pairs each token of from with its best unused partner in to, and
// divides by the larger token count so unmatched tokens on either side cost.
func greedyScore(from, to []string) float64 {
	used := make([]bool, len(to))
	total := 0.0
	for _, f := range from {
		best, at := 0.0, -1
		for i, t := range to {
			if used[i] {
				continue
			}
			if s := tokenScore(f, t); s > best {
				best, at = s, i
			}
		}
		if at >= 0 {
			used[at] = true
			total += best
		}
	}
	return total / float64(max(len(from), len(to)))
}

// tokenScore is 1 for equal tokens, 0.5 to 1 for an abbreviation by coverage of the
// longer token, and 0 otherwise. A truncation ("deploy") and a contraction ("db")
// are both an in-order subsequence; requiring the same first letter excludes
// accidental subsequences.
func tokenScore(a, b string) float64 {
	if a == b {
		return 1
	}
	short, long := []rune(a), []rune(b)
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < minAbbreviation || !abbreviates(short, long) {
		return 0
	}
	return 0.5 + 0.5*float64(len(short))/float64(len(long))
}

// abbreviates reports whether short reads as an abbreviation of long: same first
// rune, and every rune of short appearing in long in order.
func abbreviates(short, long []rune) bool {
	if short[0] != long[0] {
		return false
	}
	at := 0
	for _, r := range long {
		if r == short[at] {
			if at++; at == len(short) {
				return true
			}
		}
	}
	return false
}
