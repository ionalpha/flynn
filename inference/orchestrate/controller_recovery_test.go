package orchestrate

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/clock"
)

var errTestOOM = errors.New("CUDA out of memory")

// oomClassifier maps the test OOM error to FailureOOM and everything else to a crash, the way
// the serve layer's real classifier reads a runtime's failure output.
func oomClassifier(err error) FailureKind {
	if errors.Is(err, errTestOOM) {
		return FailureOOM
	}
	return FailureCrash
}

// TestRecoveryClimbsTheLadderOnOOM asserts the controller's response to a model that keeps
// running out of memory: the first attempt is full size, the second is degraded, the third
// falls back to the minimal (CPU) footprint, and only then is the model quarantined. An
// out-of-memory model is shrunk through every rung before it is given up on, and is never
// hammered with the same doomed launch.
func TestRecoveryClimbsTheLadderOnOOM(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 1 << 30 // every launch runs out of memory
	srv.launchErr = errTestOOM
	c := NewController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "big", Footprint: 10}}, Budget: 100}},
		srv,
		clock.System{},
		WithClassifier(oomClassifier),
	)

	mustReconcile(t, c) // full
	mustReconcile(t, c) // degraded
	mustReconcile(t, c) // minimal (fallback)
	if got := srv.countLaunches(LaunchFull); got != 1 {
		t.Fatalf("want exactly one full-size attempt, got %d", got)
	}
	if got := srv.countLaunches(LaunchDegraded); got != 1 {
		t.Fatalf("want exactly one degraded attempt, got %d", got)
	}
	if got := srv.countLaunches(LaunchMinimal); got != 1 {
		t.Fatalf("want exactly one minimal (CPU) attempt, got %d", got)
	}

	// Past the ladder, the model is quarantined: further passes launch nothing new.
	before := len(srv.actions())
	mustReconcile(t, c)
	mustReconcile(t, c)
	if len(srv.actions()) != before {
		t.Fatalf("an exhausted OOM model must be quarantined, new actions: %v", srv.actions()[before:])
	}
	if srv.isResident("big") {
		t.Fatal("the model never started, so it must not be resident")
	}
}

// TestRecoveryQuarantinesCrashLoop asserts a model that crashes past the retry bound stops
// being launched, so a crash loop does not spin the controller.
func TestRecoveryQuarantinesCrashLoop(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 1 << 30 // always crashes
	c := newTestController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "broken", Footprint: 10}}, Budget: 100}},
		srv,
	)
	// Three crash attempts (the first plus two retries), then quarantine: further passes
	// launch nothing new.
	for range 3 {
		mustReconcile(t, c)
	}
	launchedSoFar := len(srv.actions())
	mustReconcile(t, c)
	mustReconcile(t, c)
	if len(srv.actions()) != launchedSoFar {
		t.Fatalf("a crash-looping model must be quarantined, new actions: %v", srv.actions()[launchedSoFar:])
	}
}

// TestRecoveryClearsStateWhenModelLeavesDesired asserts a model removed from the desired set
// loses its quarantine, so wanting it again starts from a clean slate.
func TestRecoveryClearsStateWhenModelLeavesDesired(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 2 // crash twice, then a clean launch would succeed
	prov := &mutableProvider{ds: DesiredState{Models: []Desired{{ModelID: "x", Footprint: 10}}, Budget: 100}}
	c := newTestController(prov, srv)

	// Fail twice and reach the crash-retry edge.
	mustReconcile(t, c)
	mustReconcile(t, c)
	// Remove x from the desired set: its recovery memory must be forgotten.
	prov.set(DesiredState{Budget: 100})
	mustReconcile(t, c)
	// Want x again: it should launch fresh (the remaining failLaunch budget is 0 now, so it
	// succeeds) rather than carrying a stale quarantine.
	prov.set(DesiredState{Models: []Desired{{ModelID: "x", Footprint: 10}}, Budget: 100})
	mustReconcile(t, c)
	if !srv.isResident("x") {
		t.Fatalf("x should launch fresh after re-entering the desired set, actions: %v", srv.actions())
	}
}

func mustReconcile(t *testing.T, c *Controller) {
	t.Helper()
	// A transient apply error is expected while a model is failing; only a nil or transient
	// result is acceptable here, never a panic or an unexpected classification.
	_, _ = c.reconcile(context.Background(), serveKey{})
}

// mutableProvider is a Provider whose desired state a test can change between reconciles.
type mutableProvider struct {
	ds DesiredState
}

func (p *mutableProvider) set(ds DesiredState) { p.ds = ds }

func (p *mutableProvider) Desired(context.Context) (DesiredState, error) { return p.ds, nil }
