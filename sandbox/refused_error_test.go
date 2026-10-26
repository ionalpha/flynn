package sandbox

import (
	"strings"
	"testing"
)

// TestRefusedErrorNamesUnknownWhenNoCauseWasSet keeps a refusal readable when a result
// carries none: the log line must still say what kind of refusal it was.
func TestRefusedErrorNamesUnknownWhenNoCauseWasSet(t *testing.T) {
	err := &RefusedError{}
	if !strings.Contains(err.Error(), string(CauseUnknown)) {
		t.Fatalf("Error() = %q, want it to name %s", err.Error(), CauseUnknown)
	}
}

// TestAlsoFilterNamesTheCauseForTheStageThatFailed holds the second cause a seccomp-filtered
// process adds: a launcher failure points at the filter, an earlier rung at the restricted
// namespaces, and a process under no filter adds nothing.
func TestAlsoFilterNamesTheCauseForTheStageThatFailed(t *testing.T) {
	if got := alsoFilter(failure{stage: stageLauncher}, evidence{}); got != nil {
		t.Fatalf("no filter: got %v, want none", got)
	}
	if got := alsoFilter(failure{stage: stageLauncher}, evidence{filter: true}); len(got) != 1 || got[0] != CauseSeccompConflict {
		t.Fatalf("launcher stage: got %v, want %s", got, CauseSeccompConflict)
	}
	if got := alsoFilter(failure{stage: stageUser}, evidence{filter: true}); len(got) != 1 || got[0] != CauseNamespaceRestricted {
		t.Fatalf("user stage: got %v, want %s", got, CauseNamespaceRestricted)
	}
}
