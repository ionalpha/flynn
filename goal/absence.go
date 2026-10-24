package goal

import (
	"errors"
	"strings"
	"unicode"
)

// ErrInvariantUnsearchable reports a term that asserts an absence ("no credentials remain
// in the tree") and declares no way to find a counterexample. A presence claim has a
// witness, the run that produced the thing claimed. An absence claim has none: "I could
// not find any" is worth what the search behind it was worth, and with no search a run
// that never looked and a run that swept the tree file the same sentence. So the claim
// must arrive with its search, and a goal stating one without it is refused at admission.
//
// Two checks hold the rule and neither is trusted alone: [AssertsAbsence], which reads
// the author's words with no model in it, and the model-backed auditor, which refuses any
// prose term it reads as an absence claim. A term that does not reduce to a search is
// stated in the wrong direction; its presence form ("every identifier in the export
// appears in the allowlist") is checkable.
var ErrInvariantUnsearchable = errors.New("goal: an invariant asserting an absence must declare the search that would find one")

// absenceWords are the single words that mark a statement as a claim about what is not
// there. The list is generous: a false positive costs the author one line of check, and
// a false negative is a term that reads as a guard and cannot be one.
//
// Matching is on whole words ("no" does not fire on "north"). Contractions are matched by
// suffix, which covers the family (isn't, won't, mustn't) without listing it.
var absenceWords = map[string]bool{
	// Direct negation.
	"no": true, "none": true, "not": true, "never": true, "neither": true, "nor": true,
	"nothing": true, "nobody": true, "nowhere": true, "cannot": true, "without": true,
	// Absence stated as a property.
	"absent": true, "absence": true, "empty": true, "devoid": true, "zero": true,
	// Absence stated as the outcome of an action, which is the same claim.
	"removed": true, "deleted": true, "erased": true, "stripped": true, "purged": true,
	"redacted": true, "excluded": true, "eliminated": true, "scrubbed": true,
	// Absence of change, which is the shape the destructive-state failures take.
	"unchanged": true, "untouched": true, "unmodified": true, "unaffected": true,
}

// absencePhrases are two-word forms whose first word is ordinary on its own: "free" and
// "clear" claim absence only in this company.
var absencePhrases = [][2]string{
	{"free", "of"}, {"free", "from"}, {"clear", "of"}, {"rid", "of"}, {"void", "of"},
}

// AssertsAbsence reports whether a statement claims something is not there. It reads the
// author's words with no model involved, so the rule that an absence claim needs a search
// does not depend on a model's cooperation at admission.
//
// It over-reports on purpose. "The credentials were removed" is caught, and so is "the
// output is not empty", a presence claim wearing a negation; the second author is asked
// for a trivial search. Writing a check you did not strictly need costs a line, and being
// waved through on a term nobody can check costs the guard.
func AssertsAbsence(statement string) bool {
	words := statementWords(statement)
	for i, w := range words {
		if absenceWords[w] || strings.HasSuffix(w, "n't") || strings.HasSuffix(w, "n’t") {
			return true
		}
		for _, p := range absencePhrases {
			if w == p[0] && i+1 < len(words) && words[i+1] == p[1] {
				return true
			}
		}
	}
	return false
}

// statementWords lowercases a statement and splits it into words, keeping the apostrophe
// so a contraction stays one word. Both apostrophes count, because a statement pasted
// from a document carries the typographic one. Anything else that is not a letter or a
// digit separates, including the hyphen in "up-to-date" and the slash in "and/or".
func statementWords(statement string) []string {
	return strings.FieldsFunc(strings.ToLower(statement), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
}
