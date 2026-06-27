package orchestrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// driveToConvergence runs reconcile passes until one succeeds with no error or the cap is hit,
// modelling the controller's retry loop without its timing. It returns the number of passes.
func driveToConvergence(t *testing.T, c *Controller, maxPasses int) int {
	t.Helper()
	for i := 1; i <= maxPasses; i++ {
		if _, err := c.reconcile(context.Background(), serveKey{}); err == nil {
			return i
		}
	}
	t.Fatalf("did not converge within %d passes", maxPasses)
	return 0
}

// TestConvergesDespiteFlakyLaunch asserts that a runtime which fails to start a few times is
// retried until the model is resident, and that once converged the loop takes no further
// action. Each failed launch surfaces as a transient error, the signal the queue backs off on.
func TestConvergesDespiteFlakyLaunch(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 3
	c := newTestController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}},
		srv,
	)

	driveToConvergence(t, c, 10)
	if !srv.isResident("a") {
		t.Fatal("model never became resident despite retries")
	}
	// Idempotent once converged: a further pass succeeds and does nothing new.
	before := len(srv.actions())
	if _, err := c.reconcile(context.Background(), serveKey{}); err != nil {
		t.Fatalf("converged reconcile errored: %v", err)
	}
	if len(srv.actions()) != before {
		t.Fatalf("a converged reconcile took action: %v", srv.actions())
	}
}

// TestEvictFailureIsRetriedNotAbandoned asserts a failing evict keeps the pass transient (so it
// is retried) while still attempting the rest of the plan, and that recovery converges once the
// runtime stops failing.
func TestEvictFailureIsRetriedNotAbandoned(t *testing.T) {
	srv := newRecordingServer(Resident{ModelID: "stuck", Footprint: 10})
	srv.evictErr = errors.New("runtime will not stop")
	c := newTestController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}},
		srv,
	)

	// While eviction fails, the pass is transient and "stuck" stays resident; the desired
	// model is still launched (the plan is attempted in full).
	if _, err := c.reconcile(context.Background(), serveKey{}); fault.Classify(err) != fault.Transient {
		t.Fatalf("a failing evict must keep the pass transient, got %v", err)
	}
	if !srv.isResident("stuck") || !srv.isResident("a") {
		t.Fatalf("expected stuck retained and a launched, actions=%v", srv.actions())
	}

	// The runtime recovers; convergence completes and "stuck" is evicted.
	srv.mu.Lock()
	srv.evictErr = nil
	srv.mu.Unlock()
	driveToConvergence(t, c, 5)
	if srv.isResident("stuck") {
		t.Fatal("stuck should have been evicted once the runtime recovered")
	}
}

// TestRunSurvivesPersistentFailure asserts the live loop keeps running (backing off, not
// spinning or panicking) when an apply never succeeds, and still shuts down cleanly on cancel.
func TestRunSurvivesPersistentFailure(t *testing.T) {
	srv := newRecordingServer()
	srv.failLaunch = 1 << 30 // never succeeds
	c := NewController(
		fakeProvider{ds: DesiredState{Models: []Desired{{ModelID: "a", Footprint: 10}}, Budget: 100}},
		srv,
		clock.System{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel under persistent failure")
	}
}
