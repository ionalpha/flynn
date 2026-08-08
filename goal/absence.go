package goal

import (
	"errors"
	"strings"
	"unicode"
)

// A negative-space assertion is a term stated as an absence: no credentials remain in the
// tree, nothing outside the workspace was modified, the user's saved settings were never
// deleted. Ruling on one is a different job from ruling on a presence claim, and the
// difference is why this file exists.
//
// A presence claim has a witness. "The suite passes" is answered by the run that passed
// it: the thing claimed is the thing produced, and it is in the record. An absence claim
// has no witness, because nothing in the record is the absence. "I could not find any" is
// a statement about a search, and it is worth what that search was worth. Behind no search
// at all it is worth nothing, and it fails in exactly the case the term exists for: a run
// that never looked and a run that swept the tree file the same sentence.
//
// The reported failure is exactly that shape. A model asked to remove all personal
// identifiers removed most and reported done, and being shown one instance it had missed
// did not make it sweep for the rest. So the rule is not that absence claims get audited
// harder. It is that an absence claim must arrive with the search that would find a
// counterexample, and a goal stating one without that search is refused at admission,
// before any work is dispatched against terms nobody could ever check.
//
// Two independent layers hold the rule, and neither is trusted alone. Admission runs the
// recognizer below, which reads the author's own words and has no model in it. The
// model-backed auditor that rules on prose terms refuses any term it reads as an absence
// claim rather than passing it, which catches what the recognizer's word list missed. A
// term slips through only if the wording defeats the recognizer and the auditor also
// declares it a presence claim, and the two do not fail the same way.

// ErrInvariantUnsearchable reports a term that asserts an absence and declares no way to
// find a counterexample. It is an authoring fault, and the fix is one line: write the
// search. A term saying no credentials remain is a grep; a term saying nothing outside the
// workspace changed is a diff. Where the term genuinely does not reduce to a search, it is
// being stated in the wrong direction, and the presence form of it ("every identifier in
// the export appears in the allowlist") is both checkable and what was actually meant.
var ErrInvariantUnsearchable = errors.New("goal: an invariant asserting an absence must declare the search that would find one")

// absenceWords are the single words that mark a statement as a claim about what is not
// there. The list is deliberately generous: a false positive costs the author one line of
// check, while a false negative is a term that reads as a guard and cannot be one.
//
// Matching is on whole words, so "no" does not fire on "north" and "not" does not fire on
// "notify". Contractions are handled separately by their suffix, which covers the whole
// family (isn't, won't, mustn't) without listing it.
var absenceWords = map[string]bool{
	// Direct negation.
	"no": true, "none": true, "not": true, "never": true, "neither": true, "nor": true,
	"nothing": true, "nobody": true, "nowhere": true, "cannot": true, "without": true,
	// Absence stated as a property.
	"absent": true, "absence": true, "empty": true, "devoid": true, "zero": true,
	// Absence stated as the outcome of an action: what is claimed is that the thing is
	// gone, which is the same claim and needs the same search.
	"removed": true, "deleted": true, "erased": true, "stripped": true, "purged": true,
	"redacted": true, "excluded": true, "eliminated": true, "scrubbed": true,
	// Absence of change, which is the shape the destructive-state failures take.
	"unchanged": true, "untouched": true, "unmodified": true, "unaffected": true,
}

// absencePhrases are two-word forms whose first word is ordinary on its own. "free" and
// "clear" are praise in most sentences and a negative-space claim only in this company.
var absencePhrases = [][2]string{
	{"free", "of"}, {"free", "from"}, {"clear", "of"}, {"rid", "of"}, {"void", "of"},
}

// AssertsAbsence reports whether a statement claims something is not there. It reads the
// author's words with no model involved, which is the point: it runs at admission, before
// any model has been asked anything, so the rule that an absence claim needs a search does
// not itself depend on a model's cooperation.
//
// It over-reports on purpose. "The credentials were removed" is caught, and so is "the
// output is not empty", which is a presence claim wearing a negation. Both authors are
// asked for the search that settles the term, and for the second the search is trivial.
// The asymmetry is the design: being asked to write a check you did not strictly need
// costs a line, and being waved through on a term nobody can check costs the guard.
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
// so a contraction stays one word and its suffix is recognisable. Both apostrophes count,
// because a statement pasted out of a document carries the typographic one and "doesn’t"
// is the same word as "doesn't". Everything else that is not a letter or a digit
// separates: punctuation, the hyphen in "up-to-date", the slash in "and/or".
func statementWords(statement string) []string {
	return strings.FieldsFunc(strings.ToLower(statement), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
}
