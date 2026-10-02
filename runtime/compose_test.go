package runtime

import (
	"testing"

	"github.com/ionalpha/flynn/goal"
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
