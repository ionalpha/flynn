package goal

import (
	"errors"
	"strings"
	"unicode"
)

// ErrInvariantUnsearchable refuses an absence claim with no search at admission, because
// "I found none" is worth only the search behind it. [AssertsAbsence] and the model auditor
// both enforce it. A term with no search is usually checkable in its presence form.
var ErrInvariantUnsearchable = errors.New("goal: an invariant asserting an absence must declare the search that would find one")

// absenceWords is generous because a false positive costs one line of check and a false
// negative is a guard that cannot guard. Contractions match by suffix instead.
var absenceWords = map[string]bool{
	// Direct negation.
	"no": true, "none": true, "not": true, "never": true, "neither": true, "nor": true,
	"nothing": true, "nobody": true, "nowhere": true, "cannot": true, "without": true,
	// Absence stated as a property.
	"absent": true, "absence": true, "empty": true, "devoid": true, "zero": true,
	// Absence stated as the outcome of an action, which is the same claim.
	"removed": true, "deleted": true, "erased": true, "stripped": true, "purged": true,
	"redacted": true, "excluded": true, "eliminated": true, "scrubbed": true,
	// Absence of change, the form destructive-state failures take.
	"unchanged": true, "untouched": true, "unmodified": true, "unaffected": true,
}

// absencePhrases claim absence only as a pair.
var absencePhrases = [][2]string{
	{"free", "of"}, {"free", "from"}, {"clear", "of"}, {"rid", "of"}, {"void", "of"},
}

// AssertsAbsence uses no model, so the rule does not depend on a model's cooperation. It
// over-reports: "the output is not empty" is caught too, and costs its author a trivial
// search.
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

// statementWords keeps both apostrophes so a contraction stays one word, because pasted
// text carries the typographic one.
func statementWords(statement string) []string {
	return strings.FieldsFunc(strings.ToLower(statement), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
}
