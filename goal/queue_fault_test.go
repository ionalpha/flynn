// Package goal_test drives the goal reconciler and worker against a failing job
// queue. It lives in the external test package because the shared fault injectors
// (internal/testkit) import goal, which an internal test file cannot do.
package goal_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// errQueueDown is the sentinel every injected queue failure carries.
var errQueueDown = errors.New("goal test: queue unavailable")

// neverMet is a stop evaluator whose condition never holds, so the goal keeps
// dispatching steps and the queue stays on the critical path.
type neverMet struct{}

func (neverMet) Met(context.Context, goal.Spec, goal.Status) (bool, string, error) {
	return false, "", nil
}

// parkingExec reports that its step is waiting on external state, the path whose
// completion the failing queue below drops.
type parkingExec struct{}

func (parkingExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	return nil, goal.ErrWaiting
}

// idleExec is never expected to run: the cases here fail before any step reaches
// an executor.
type idleExec struct{ calls int }

func (e *idleExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	e.calls++
	return nil, nil
}

// queueFixture wires a store, a reconciler, and one goal over the given queue.
func queueFixture(t *testing.T, q jobs.Queue, clk clock.Clock) (resource.Store, *goal.Reconciler, reconcile.Ref) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(clk))
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw,
	}); err != nil {
		t.Fatal(err)
	}
	return store, goal.NewReconciler(store, q, clk, neverMet{}), reconcile.Ref{Kind: goal.Kind, Name: "g"}
}

func statusOf(t *testing.T, store resource.Store, ref reconcile.Ref) goal.Status {
	t.Helper()
	r, err := store.Get(context.Background(), ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// TestReconcileEnqueueFailureStalls: a queue that cannot accept the step means the
// goal can make no progress. The failure is unclassified, so it is terminal: the
// goal settles as stalled carrying the queue's cause, rather than sitting idle
// with no step in flight and nothing left to wake it.
func TestReconcileEnqueueFailureStalls(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := testkit.FaultyQueue(jobs.NewMemory(jobs.WithClock(clk)), testkit.QueueFaults{
		Enqueue: testkit.Always(errQueueDown),
	})
	store, gr, ref := queueFixture(t, q, clk)

	if _, err := gr.Reconcile(ctx, ref); err != nil {
		t.Fatalf("a dead queue must settle the goal, not error: %v", err)
	}

	st := statusOf(t, store, ref)
	if st.Phase != goal.PhaseStalled {
		t.Fatalf("a goal whose step could not be enqueued is %q, want Stalled", st.Phase)
	}
	if !strings.Contains(st.Message, errQueueDown.Error()) {
		t.Fatalf("stall message = %q, want the queue failure", st.Message)
	}
	if st.InFlight != nil {
		t.Fatal("a step that was never enqueued was recorded in flight")
	}
}

// TestWorkerClaimFailurePropagates: a queue that cannot be read claims nothing.
// The worker reports the failure and no step is reported processed, so a Run loop
// waits out its poll instead of spinning on a dead queue.
func TestWorkerClaimFailurePropagates(t *testing.T) {
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := testkit.FaultyQueue(jobs.NewMemory(jobs.WithClock(clk)), testkit.QueueFaults{
		Claim: testkit.Always(errQueueDown),
	})
	exec := &idleExec{}
	w := goal.NewWorker(resource.NewMemory(resource.NewRegistry()), q, clk, exec)

	processed, err := w.ProcessOnce(context.Background())
	if !errors.Is(err, errQueueDown) {
		t.Fatalf("ProcessOnce error = %v, want the claim failure", err)
	}
	if processed {
		t.Fatal("a failed claim reported a processed step")
	}
	if exec.calls != 0 {
		t.Fatalf("the executor ran %d times without a claimed step", exec.calls)
	}
}

// TestWorkerWaitingStepReportsCompleteFailure: a parked step whose completion
// cannot be recorded must surface the failure. Reporting success would leave the
// job claimed with nobody working it, and the goal would only recover once the
// lease lapsed. The wait mark is still written first, so the re-run of the step
// does not count against the budget.
func TestWorkerWaitingStepReportsCompleteFailure(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := testkit.FaultyQueue(jobs.NewMemory(jobs.WithClock(clk)), testkit.QueueFaults{
		Complete: testkit.Always(errQueueDown),
	})
	store, gr, ref := queueFixture(t, q, clk)
	if _, err := gr.Reconcile(ctx, ref); err != nil { // finalizer + dispatch
		t.Fatalf("reconcile: %v", err)
	}

	w := goal.NewWorker(store, q, clk, parkingExec{}, goal.WithLease(time.Minute))
	processed, err := w.ProcessOnce(ctx)
	if !processed {
		t.Fatal("the step was not claimed")
	}
	if !errors.Is(err, errQueueDown) {
		t.Fatalf("ProcessOnce error = %v, want the failed completion", err)
	}
	if st := statusOf(t, store, ref); st.WaitingSince == nil {
		t.Fatal("the waiting mark was not recorded before the completion failed")
	}
}

// TestRunStopsWhileIdleOnAQueueWithoutAWaker: a queue that cannot signal readiness
// leaves the poll as the only wake-up, and the loop must still shut down when its
// context is cancelled mid-wait rather than sleeping out the poll. The fault
// wrapper is exactly such a queue: it forwards the jobs.Queue methods and not the
// readiness signal.
func TestRunStopsWhileIdleOnAQueueWithoutAWaker(t *testing.T) {
	q := testkit.FaultyQueue(jobs.NewMemory(), testkit.QueueFaults{})
	if _, ok := q.(jobs.Waker); ok {
		t.Fatal("the wrapped queue signals readiness; this case no longer covers the poll-only loop")
	}
	// A real clock is deliberate here: the loop's idle wait is a timer, and the
	// property under test is that cancellation, not the timer, ends it.
	w := goal.NewWorker(resource.NewMemory(resource.NewRegistry()), q, clock.System{}, &idleExec{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, 50*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Let the loop idle through a poll tick or two, then cancel: the wait ends on
	// the cancellation, well inside the deadline below.
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle Run did not return when its context was cancelled")
	}
}
