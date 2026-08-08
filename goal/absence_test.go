package goal

import (
	"errors"
	"testing"
)

// TestAssertsAbsenceReadsTheAuthorsWords covers the recognizer that decides whether a term
// needs a search. It runs at admission with no model in it, which is the property the two
// layers of the absence rule depend on: this half cannot be talked out of its answer.
func TestAssertsAbsenceReadsTheAuthorsWords(t *testing.T) {
	absence := []string{
		"no credential leaves the workspace",
		"none of the user's saved settings are deleted",
		"never force-push a shared branch",
		"nothing outside the working directory is modified",
		"the export contains no personal identifiers",
		"every API key is removed from the tree",
		"the vendored directory is unchanged",
		"the diff is free of debugging output",
		"the config must not be rewritten",
		"the credentials file isn't touched",
		"the credentials file isn’t touched",
	}
	for _, s := range absence {
		if !AssertsAbsence(s) {
			t.Errorf("AssertsAbsence(%q) = false, want true: an absence claim needs a search", s)
		}
	}

	presence := []string{
		"the test suite passes",
		"every edit lands under the module the objective names",
		"the north field keeps its current units",
		"the operator is notified before each deploy",
		"the release notes describe every user-visible change",
		"the schema migration is reversible",
	}
	for _, s := range presence {
		if AssertsAbsence(s) {
			t.Errorf("AssertsAbsence(%q) = true, want false", s)
		}
	}
}

// TestAnAbsenceClaimNeedsItsSearch is the rule itself: a term saying something is not
// there is refused at validation unless it declares how a counterexample would be found,
// and the same term with a search is fine. The refusal is the whole point, because the
// term it refuses is the one that would otherwise sail through: nothing in a run's record
// is the absence, so a run that never looked reports what a run that swept reports.
func TestAnAbsenceClaimNeedsItsSearch(t *testing.T) {
	bare := []Invariant{{ID: "no-secrets", Statement: "no credential leaves the workspace"}}
	err := ValidateInvariants(bare)
	if !errors.Is(err, ErrInvariantUnsearchable) {
		t.Fatalf("ValidateInvariants = %v, want ErrInvariantUnsearchable", err)
	}

	searched := []Invariant{{
		ID:        "no-secrets",
		Statement: "no credential leaves the workspace",
		Check:     "! git grep -qE 'AKIA[0-9A-Z]{16}'",
	}}
	if err := ValidateInvariants(searched); err != nil {
		t.Fatalf("a term declaring its search was refused: %v", err)
	}
}

// TestAWhitespaceCheckIsNoSearch: the search has to be written, not gestured at. A check
// field holding a newline satisfies nothing, and the auditor that would run it treats it
// the same way, so admission has to agree or the goal starts and stalls a step later.
func TestAWhitespaceCheckIsNoSearch(t *testing.T) {
	err := ValidateInvariants([]Invariant{{
		ID:        "no-secrets",
		Statement: "no credential leaves the workspace",
		Check:     "  \n\t ",
	}})
	if !errors.Is(err, ErrInvariantUnsearchable) {
		t.Fatalf("ValidateInvariants = %v, want ErrInvariantUnsearchable", err)
	}
}
