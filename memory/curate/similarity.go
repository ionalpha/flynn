package curate

import "strings"

// minAbbreviation is the shortest prefix treated as an abbreviation of a longer
// token. One character is not evidence of anything: "d" prefixes "deploy",
// "database" and "docs" alike, and scoring it as a near-match would report a fork
// between every subject starting with the same letter.
const minAbbreviation = 2

// SubjectSimilarity scores how alike two normalized subjects look, in [0,1] with 1
// identical. It is the default measure behind fork detection; a host with embeddings
// replaces it (WithSimilarity).
//
// It compares subjects token by token, because the forks that matter share a word and
// differ in one: "db-choice" against "database-choice" is a fork, "db-choice" against
// "queue-choice" is two topics, and a character measure over the whole string scores the
// second pair higher. Within a token an abbreviation is a partial match ("db" for
// "database"), scoring lower as it gets shorter relative to the word.
//
// The score is symmetric: the greedy pairing runs both ways and the lower score wins, so
// the answer does not depend on which subject the store listed first, and the conservative
// direction suits a signal that costs a person attention each time it fires. It cannot see
// through a synonym ("db-choice" and "storage-engine"); only an embedding measure closes that.
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

// tokenScore scores one token against another: 1 for equal, a fraction for an
// abbreviation, 0 otherwise. The fraction starts at a half so an abbreviation is
// always worth less than a match, and rises with how much of the longer token the
// shorter one covers, so a two-letter stand-in for a long word scores near the
// floor.
//
// Abbreviation covers both shapes people actually write: a truncation
// ("deployment" to "deploy") and a contraction ("database" to "db", which is not
// a prefix of anything). Both are read as the short token appearing inside the
// long one in order, starting from the same letter. The shared first letter is
// what keeps an accidental subsequence out: without it, almost any short token is
// a subsequence of almost any long one.
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
