package goal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// enqueuePlan queues a plan job for the given goal id directly, without a reconcile
// pass. It is how the worker's plan path is reached over a record the reconciler
// would never dispatch a plan for on its own (an undecodable status).
func enqueuePlan(t *testing.T, q jobs.Queue, goalID string) {
	t.Helper()
	if _, err := q.Enqueue(context.Background(), jobs.EnqueueParams{
		Queue: StepQueue, Kind: PlanJobKind, Payload: []byte(goalID),
	}); err != nil {
		t.Fatalf("enqueue plan: %v", err)
	}
}

// TestPlanStepFailsWhenThePlannerErrors: a planner that cannot expand the objective
// fails the plan job with the cause rather than marking the goal planned against a
// ledger that was never produced. The reconciler then settles it as stalled.
func TestPlanStepFailsWhenThePlannerErrors(t *testing.T) {
	p := &fakePlanner{err: errors.New("model refused to plan")}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.reconcile(t, ref) // dispatch the plan job
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref) // observe the failed plan

	if st := h.status(t, ref); st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled after the planner errored", st.Phase)
	}
	if p.calls != 1 {
		t.Fatalf("planner ran %d times, want 1", p.calls)
	}
}

// TestPlanStepFailsWhenTheItemsCannotBeRecorded: a planner whose items cannot be
// admitted to the ledger (here, two items with identical content, which the append
// rule refuses as a duplicate) fails the plan job rather than recording a malformed
// ledger. The write is not best-effort the way a checkpoint is: a ledger that fails
// to land must fail loudly, not leave the goal silently unplanned.
func TestPlanStepFailsWhenTheItemsCannotBeRecorded(t *testing.T) {
	p := &fakePlanner{items: []LedgerItem{
		{Item: "a", Verify: "check a"},
		{Item: "a", Verify: "check a"},
	}}
	h := newPlanHarness(t, neverStop{}, p)
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c"})

	h.reconcile(t, ref)
	if _, err := h.w.ProcessOnce(h.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	h.reconcile(t, ref)

	if st := h.status(t, ref); st.Phase != PhaseStalled {
		t.Fatalf("phase = %q, want Stalled after an unrecordable plan", st.Phase)
	}
	if sp := h.spec(t, ref); len(sp.Ledger) != 0 {
		t.Fatalf("a malformed plan left %d items on the ledger, want none", len(sp.Ledger))
	}
}

// TestPlanStepFailsOnAnUndecodableStatus: recording a plan reads the goal back under
// the conflict-retry policy and decodes its status. A status that cannot be decoded
// fails the plan job instead of writing a ledger against a record the worker cannot
// read. Only a status corrupted around the store's write-path validation can reach
// this, so it is driven directly.
func TestPlanStepFailsOnAnUndecodableStatus(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	q := jobs.NewMemory(jobs.WithClock(m))
	h, fs := faultHarness(t, q, neverStop{}, WithPlanning())
	created := putRaw(t, fs, resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "g",
		Spec:   goalSpec(t, Spec{Objective: "o", StopCondition: "c"}),
		Status: json.RawMessage(`"not a status object"`),
	})
	enqueuePlan(t, q, created.ID)

	p := &fakePlanner{items: []LedgerItem{{Item: "a", Verify: "check a"}}}
	w := NewWorker(fs, q, m, &fakeExec{}, WithPlanner(p), WithLease(time.Minute))

	processed, err := w.ProcessOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("the plan step should be claimed and failed, not error out: processed=%v err=%v", processed, err)
	}
	if p.calls != 1 {
		t.Fatalf("planner ran %d times, want 1 (the failure is in recording, not planning)", p.calls)
	}

	// The ledger write never landed: nothing was recorded against the record whose
	// status could not be decoded.
	r, err := fs.Store.GetByID(h.ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Ledger) != 0 {
		t.Fatalf("a ledger was recorded against an undecodable status: %+v", sp.Ledger)
	}
}
