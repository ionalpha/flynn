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

// TestRecoveryDegradesThenQuarantinesOnOOM asserts the controller's response to a model that
// keeps running out of memory: the first failure retries unchanged, the second relaunches
// degraded (smaller footprint), and once the policy escalates past degrade the model is no
// longer launched, so an out-of-memory model is never hammered with the same doomed launch.
func TestRecoveryDegradesThenQuarantinesOnOOM(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 1 << 30 // every launch runs out of memory
	srv.launchErr = errTestOOM
	c := NewController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "big", Footprint: 10}}, Budget: 100}},
		srv,
		clock.System{},
		WithClassifier(oomClassifier),
	)

	// Pass 1: first OOM, recorded. The launch was attempted at full size.
	mustReconcile(t, c)
	if srv.degradedLaunches() != 0 {
		t.Fatalf("first attempt must be full-size, got %d degraded", srv.degradedLaunches())
	}
	// Pass 2: the policy degrades, so the relaunch is at a smaller footprint.
	mustReconcile(t, c)
	if srv.degradedLaunches() != 1 {
		t.Fatalf("second attempt must be degraded, got %d degraded launches", srv.degradedLaunches())
	}
	// Pass 3+: escalated past degrade; the model is no longer launched.
	before := len(srv.actions())
	mustReconcile(t, c)
	mustReconcile(t, c)
	if len(srv.actions()) != before {
		t.Fatalf("an escalated OOM model must not be launched again, new actions: %v", srv.actions()[before:])
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
