package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

// everyPort stands in for every optional port at once. Its embedded interfaces are nil,
// so calling a method would panic; composition calls none of them.
type everyPort struct {
	goal.Planner
	goal.ProgressProbe
	goal.UnitSpawner
	goal.InvariantAuditor
	goal.SteerJudge
	goal.RefusalProbe
}

// TestNewComposesEveryOptionalPortTogether: each optional port is wired by its own
// branch of New, and a host that supplies all of them at once must still get a
// runtime.
func TestNewComposesEveryOptionalPortTogether(t *testing.T) {
	ports := everyPort{}
	rt, err := New(Config{
		Executor:           stubExec{},
		Stop:               stubStop{},
		Planner:            ports,
		Progress:           ports,
		Units:              ports,
		Auditor:            ports,
		SteerJudge:         ports,
		Refusals:           ports,
		PollInterval:       1,
		StepMaxAttempts:    2,
		WorkerLease:        1,
		WorkerRetryBase:    1,
		Resync:             1,
		DriveSubmittedOnly: true,
	})
	if err != nil {
		t.Fatalf("New with every optional port set: %v", err)
	}
	if rt == nil {
		t.Fatal("New returned no runtime and no error")
	}
}

// TestNewRefusesAStoreWithoutItsQueue: a host that supplies a store must supply the
// job queue that goes with it, rather than having an in-memory one paired with it.
func TestNewRefusesAStoreWithoutItsQueue(t *testing.T) {
	store := resource.NewMemory(resource.NewRegistry())
	if _, err := New(Config{Executor: stubExec{}, Stop: stubStop{}, Store: store}); err == nil {
		t.Fatal("New accepted a store with no job queue")
	}
}

// TestNewRefusesAGateThatFailsItsSelfTest: a gate that cannot show it refuses a claim
// with no evidence must stop the runtime from being built, rather than be wired in to
// certify every claim.
func TestNewRefusesAGateThatFailsItsSelfTest(t *testing.T) {
	prior := newEvidenceGate
	t.Cleanup(func() { newEvidenceGate = prior })
	newEvidenceGate = func(...goal.GateOption) (*goal.EvidenceGate, error) {
		return nil, errors.New("self-test: admitted a claim with no evidence")
	}
	_, err := New(Config{Executor: stubExec{}, Stop: stubStop{}, Verifier: stubVerifier{}, Evidence: stubEvidence{}})
	if err == nil || !strings.Contains(err.Error(), "evidence gate") {
		t.Fatalf("New with a failing gate = %v, want an evidence gate error", err)
	}
}
