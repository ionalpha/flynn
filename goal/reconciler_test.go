package goal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// --- fakes ------------------------------------------------------------------

// stopAfter reports the stop condition met once the goal has taken `at` steps.
type stopAfter struct{ at int }

func (s stopAfter) Met(_ context.Context, _ Spec, st Status) (bool, string, error) {
	if st.Steps >= s.at {
		return true, "reached step target", nil
	}
	return false, "", nil
}

// recordingCleaner counts cleanup calls and can fail a given number of times first.
type recordingCleaner struct {
	calls   int
	failFor int
}

func (c *recordingCleaner) Cleanup(context.Context, resource.Resource) error {
	c.calls++
	if c.calls <= c.failFor {
		return errCleanup
	}
	return nil
}

var errCleanup = &cleanupErr{}

type cleanupErr struct{}

func (*cleanupErr) Error() string { return "cleanup not done" }

// --- harness ----------------------------------------------------------------

type harness struct {
	ctx   context.Context
	store resource.Store
	jobs  *jobs.MemoryQueue
	gr    *Reconciler
	clk   *clock.Manual
}

func newHarness(t *testing.T, stop StopEvaluator, opts ...Option) *harness {
	t.Helper()
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg, resource.WithClock(m))
	q := jobs.NewMemory()
	gr := NewReconciler(store, q, m, stop, opts...)
	return &harness{ctx: context.Background(), store: store, jobs: q, gr: gr, clk: m}
}

func (h *harness) createGoal(t *testing.T, name string, spec Spec) reconcile.Ref {
	t.Helper()
	raw, _ := json.Marshal(spec)
	if _, err := h.store.Put(h.ctx, resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: name, Spec: raw,
	}); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	return reconcile.Ref{Kind: Kind, Name: name}
}

func (h *harness) reconcile(t *testing.T, ref reconcile.Ref) reconcile.Result {
	t.Helper()
	res, err := h.gr.Reconcile(h.ctx, ref)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *harness) status(t *testing.T, ref reconcile.Ref) Status {
	t.Helper()
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// completeStep simulates a worker finishing the dispatched step.
func (h *harness) completeStep(t *testing.T) {
	t.Helper()
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("no step job to claim (err=%v)", err)
	}
	if claimed[0].Kind != StepJobKind {
		t.Fatalf("claimed job kind = %q, want %q", claimed[0].Kind, StepJobKind)
	}
	if err := h.jobs.Complete(h.ctx, claimed[0].ID); err != nil {
		t.Fatalf("complete step: %v", err)
	}
}

// failStepToDeath claims the step and fails it until it goes dead.
func (h *harness) failStepToDeath(t *testing.T) {
	t.Helper()
	claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("no step job to claim (err=%v)", err)
	}
	if err := h.jobs.Fail(h.ctx, claimed[0].ID, "boom", h.clk.Now().UnixNano()); err != nil {
		t.Fatalf("fail step: %v", err)
	}
}

func hasCond(st Status, typ, status string) bool {
	for _, c := range st.Conditions {
		if c.Type == typ {
			return c.Status == status
		}
	}
	return false
}

// --- tests ------------------------------------------------------------------

func TestGoalConvergence(t *testing.T) {
	h := newHarness(t, stopAfter{at: 3})
	ref := h.createGoal(t, "ship", Spec{Objective: "ship it", StopCondition: "shipped"})

	h.reconcile(t, ref) // adds the finalizer
	r, _ := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if !hasFinalizer(r.Finalizers, Finalizer) {
		t.Fatal("first reconcile did not add the finalizer")
	}

	h.reconcile(t, ref) // dispatches step 1
	st := h.status(t, ref)
	if st.InFlight == nil || st.Phase != PhaseRunning || st.Steps != 0 {
		t.Fatalf("after dispatch: %+v", st)
	}
	if !hasCond(st, CondReconciling, "True") {
		t.Fatal("Reconciling condition not set on dispatch")
	}

	// Drive three steps to convergence.
	for i := 0; i < 3; i++ {
		h.completeStep(t)
		h.reconcile(t, ref)
	}
	st = h.status(t, ref)
	if st.Phase != PhaseConverged || st.InFlight != nil || st.Steps != 3 {
		t.Fatalf("not converged cleanly: %+v", st)
	}
	if !hasCond(st, CondReady, "True") || hasCond(st, CondReconciling, "True") {
		t.Fatalf("conditions wrong at convergence: %+v", st.Conditions)
	}
}

func TestGoalNeverDispatchesDuplicate(t *testing.T) {
	h := newHarness(t, stopAfter{at: 99})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // dispatch step 1

	// Re-reconcile WITHOUT completing the step: it must observe, not relaunch.
	res := h.reconcile(t, ref)
	if res.RequeueAfter == 0 {
		t.Fatal("in-flight reconcile should poll (RequeueAfter)")
	}
	jobsClaimed, _ := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 10, LeaseFor: int64(time.Minute)})
	if len(jobsClaimed) != 1 {
		t.Fatalf("expected exactly 1 step job in flight, got %d (duplicate dispatch)", len(jobsClaimed))
	}
}

func TestGoalBudgetStalls(t *testing.T) {
	h := newHarness(t, stopAfter{at: 99}) // never converges
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", MaxSteps: 2})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // step 1
	h.completeStep(t)
	h.reconcile(t, ref) // step 2
	h.completeStep(t)
	h.reconcile(t, ref) // observes 2 steps, budget exhausted

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
		t.Fatalf("budget did not stall the goal: %+v", st)
	}
}

func TestGoalStepFailureStalls(t *testing.T) {
	h := newHarness(t, stopAfter{at: 99}, WithStepMaxAttempts(1))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // dispatch step
	h.failStepToDeath(t)
	h.reconcile(t, ref) // observes the dead step

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
		t.Fatalf("dead step did not stall the goal: %+v", st)
	}
}

func TestGoalNoOpWhenSettled(t *testing.T) {
	h := newHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // dispatch step 1
	h.completeStep(t)
	h.reconcile(t, ref) // converge
	before := h.status(t, ref)

	// A reconcile of a settled goal with unchanged spec does nothing.
	h.reconcile(t, ref)
	after := h.status(t, ref)
	if before.Steps != after.Steps || after.InFlight != nil || after.Phase != PhaseConverged {
		t.Fatalf("settled goal was disturbed: before=%+v after=%+v", before, after)
	}
	if pending, _ := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 10, LeaseFor: int64(time.Minute)}); len(pending) != 0 {
		t.Fatalf("settled goal dispatched a new step: %d", len(pending))
	}
}

func TestGoalFinalizerCleanup(t *testing.T) {
	cleaner := &recordingCleaner{}
	h := newHarness(t, stopAfter{at: 99}, WithCleaner(cleaner))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref) // adds finalizer

	// Request deletion: the goal becomes terminating but stays live.
	if err := h.store.Delete(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatalf("terminating goal must still be readable: %v", err)
	}

	h.reconcile(t, ref) // finalize -> cleanup -> remove finalizer -> deleted
	if cleaner.calls != 1 {
		t.Fatalf("cleanup ran %d times, want 1", cleaner.calls)
	}
	if _, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name); err == nil {
		t.Fatal("goal not deleted after finalizer cleared")
	}
}

func TestGoalFinalizerBlocksOnCleanupFailure(t *testing.T) {
	cleaner := &recordingCleaner{failFor: 1} // fail once, then succeed
	h := newHarness(t, stopAfter{at: 99}, WithCleaner(cleaner))
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})
	h.reconcile(t, ref)
	if err := h.store.Delete(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatal(err)
	}

	// First finalize attempt fails cleanup: the goal stays (finalizer not removed).
	if _, err := h.gr.Reconcile(h.ctx, ref); err == nil {
		t.Fatal("failed cleanup should surface an error so the delete retries")
	}
	if _, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name); err != nil {
		t.Fatal("goal must remain while cleanup is unfinished (no leak)")
	}
	// Retry: cleanup succeeds, goal is removed.
	h.reconcile(t, ref)
	if _, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name); err == nil {
		t.Fatal("goal not deleted after cleanup eventually succeeded")
	}
}
