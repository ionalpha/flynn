package goal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// recoverAfter fails transiently failsLeft times, then succeeds. It models a step
// whose model or tool is briefly unavailable and then recovers, so the worker must
// carry the step across the outage rather than abandoning it or restarting it.
type recoverAfter struct {
	failsLeft int
	calls     int
}

func (e *recoverAfter) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	e.calls++
	if e.failsLeft > 0 {
		e.failsLeft--
		return nil, fault.New(fault.Transient, "step_flaky", "step temporarily unavailable")
	}
	return nil, nil
}

// chaosFixture wires a reconciler and worker over one shared manual clock, queue,
// and store, with a chosen executor, retry ceiling, and backoff. It mirrors the
// standalone setup the other worker tests build by hand.
func chaosFixture(t *testing.T, exec StepExecutor, maxAttempts int, base, ceil time.Duration) (*clock.Manual, *jobs.MemoryQueue, resource.Store, *Reconciler, *Worker) {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	gr := NewReconciler(store, q, m, stopAfter{at: 1}, WithStepMaxAttempts(maxAttempts))
	w := NewWorker(store, q, m, exec, WithLease(time.Minute), WithBackoff(base, ceil))
	return m, q, store, gr, w
}

// dispatchGoal creates a goal and reconciles it twice: once to add the finalizer,
// once to dispatch the first step (leaving a claimable step job).
func dispatchGoal(t *testing.T, store resource.Store, gr *Reconciler) reconcile.Ref {
	t.Helper()
	ctx := context.Background()
	raw, _ := json.Marshal(Spec{Objective: "o", StopCondition: "c"})
	r, err := store.Put(ctx, resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: "g", Spec: raw})
	if err != nil {
		t.Fatal(err)
	}
	ref := reconcile.Ref{Kind: Kind, Name: r.Name}
	mustReconcile(ctx, t, gr, ref) // finalizer
	mustReconcile(ctx, t, gr, ref) // dispatch the step
	return ref
}

func mustProcess(ctx context.Context, t *testing.T, w *Worker) {
	t.Helper()
	processed, err := w.ProcessOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("expected the step to be claimed and processed: processed=%v err=%v", processed, err)
	}
}

func phaseOf(ctx context.Context, t *testing.T, store resource.Store, ref reconcile.Ref) Phase {
	t.Helper()
	cur, err := store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	st, err := DecodeStatus(cur)
	if err != nil {
		t.Fatal(err)
	}
	return st.Phase
}

// TestWorkerRetryLadderExhaustsToStalled drives a persistently-failing step through
// its whole retry budget and asserts the worker-computed backoff grows each attempt
// (doubling), the job goes dead exactly at MaxAttempts, and the reconciler then
// stalls the goal. The existing backoff test only covers a single failed attempt;
// this pins the full ladder to death, which nothing exercised end to end (the
// reconciler helper that reaches Stalled fails the queue directly, bypassing the
// worker's backoff path).
func TestWorkerRetryLadderExhaustsToStalled(t *testing.T) {
	ctx := context.Background()
	m, _, store, gr, w := chaosFixture(t, failingExec{}, 3, 2*time.Second, time.Minute)
	ref := dispatchGoal(t, store, gr)

	// Attempt 1 fails and is rescheduled one base interval out.
	mustProcess(ctx, t, w)
	if processed, _ := w.ProcessOnce(ctx); processed {
		t.Fatal("attempt 1 was re-claimable before its backoff elapsed")
	}
	m.Advance(2 * time.Second) // base elapsed -> claimable

	// Attempt 2 fails; the backoff has doubled, so one base interval is no longer
	// enough to make it claimable again.
	mustProcess(ctx, t, w)
	m.Advance(2 * time.Second)
	if processed, _ := w.ProcessOnce(ctx); processed {
		t.Fatal("attempt 2 backoff did not grow: claimable after only one base interval")
	}
	m.Advance(2 * time.Second) // second base interval -> total 4s -> claimable

	// Attempt 3 exhausts MaxAttempts: the job goes dead and never returns.
	mustProcess(ctx, t, w)
	m.Advance(10 * time.Minute)
	if processed, _ := w.ProcessOnce(ctx); processed {
		t.Fatal("a step past MaxAttempts was retried; it must be dead")
	}

	if _, err := gr.Reconcile(ctx, ref); err != nil {
		t.Fatalf("reconcile after exhaustion: %v", err)
	}
	if got := phaseOf(ctx, t, store, ref); got != PhaseStalled {
		t.Fatalf("goal phase = %q, want Stalled after the retry ladder exhausted", got)
	}
}

// TestWorkerRecoversAfterTransientFailures proves a step that fails transiently a
// few times and then succeeds is carried to convergence: the worker retries across
// the outage and the executor is called exactly (failures + 1) times. Nothing
// covered fail-then-recover; the existing failing execs never recover.
func TestWorkerRecoversAfterTransientFailures(t *testing.T) {
	ctx := context.Background()
	exec := &recoverAfter{failsLeft: 2}
	m, _, store, gr, w := chaosFixture(t, exec, 5, 2*time.Second, time.Minute)
	ref := dispatchGoal(t, store, gr)

	converged := false
	for range 30 {
		if _, err := gr.Reconcile(ctx, ref); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if _, err := w.ProcessOnce(ctx); err != nil {
			t.Fatalf("worker: %v", err)
		}
		m.Advance(time.Minute) // clear any retry backoff before the next pass
		if phaseOf(ctx, t, store, ref) == PhaseConverged {
			converged = true
			break
		}
	}
	if !converged {
		t.Fatal("goal did not converge after the transient failures cleared")
	}
	if exec.calls != 3 {
		t.Fatalf("executor ran %d times, want 3 (2 transient failures + 1 success)", exec.calls)
	}
}

// TestWorkerFailSignalsWaker pins the wake-on-fail path: a retryable failure
// re-pends the step and must signal the Waker, so an idle worker picks up the due
// retry on the signal instead of sleeping until the next poll. The existing wake
// test only covers wake-on-enqueue.
func TestWorkerFailSignalsWaker(t *testing.T) {
	ctx := context.Background()
	_, q, store, gr, w := chaosFixture(t, failingExec{}, 5, 2*time.Second, time.Minute)
	dispatchGoal(t, store, gr)

	// Drain the enqueue signal from dispatch so the next signal we observe can only
	// be the one the worker's Fail emits.
	select {
	case <-q.Ready():
	default:
	}

	mustProcess(ctx, t, w) // claims, runs, fails, re-pends with backoff

	select {
	case <-q.Ready():
		// The retryable failure re-armed the wake signal.
	default:
		t.Fatal("a failed step did not signal the Waker; an idle worker would sleep until the poll instead of waking at RunAt")
	}
}
