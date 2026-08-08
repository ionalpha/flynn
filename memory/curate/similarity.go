package curate

import "strings"

// minAbbreviation is the shortest prefix treated as an abbreviation of a longer
// token. One character is not evidence of anything: "d" prefixes "deploy",
// "database" and "docs" alike, and scoring it as a near-match would report a fork
// between every subject starting with the same letter.
const minAbbreviation = 2

// SubjectSimilarity scores how alike two normalized subjects look, in [0,1] with
// 1 identical. It is the default measure behind fork detection, and a host with
// embeddings in hand replaces it (WithSimilarity).
//
// It compares subjects token by token rather than as strings, because the forks
// that matter share a word and differ in one: "db-choice" against
// "database-choice" is a fork, and "db-choice" against "queue-choice" is two
// topics that happen to be about a choice. A character measure over the whole
// string scores the second pair higher than the first, which is exactly backwards.
//
// Within a token, an abbreviation counts as a partial match: "db" for "database",
// "prod" for "production", "deploy" for "deployment". That is the shape these
// forks actually take, and the score falls away as the abbreviation gets shorter
// relative to the word, so a two-letter prefix of a long word scores near the
// floor rather than near a match.
//
// The score is symmetric by construction: the greedy pairing is run in both
// directions and the lower score wins, so the answer does not depend on which
// subject the store happened to list first. Being the lower of the two also makes
// it conservative, which is the right direction for a signal that costs a person
// attention every time it fires.
//
// What it cannot do is see through a synonym. "db-choice" and "storage-engine"
// are the same topic and share no token, so nothing lexical will connect them.
// That is the gap an embedding measure closes.
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
