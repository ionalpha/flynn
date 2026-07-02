package goal

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// fanoutStop drives a simulated fan-out: the parent converges once it has taken
// two real steps (the fan-out turn and the fold), and a child converges when the
// test marks it finished.
type fanoutStop struct{ childDone map[string]bool }

func (s *fanoutStop) Met(_ context.Context, spec Spec, st Status) (bool, string, error) {
	if spec.Objective == "parent" {
		return st.Steps >= 2, "folded", nil
	}
	return s.childDone[spec.Objective], "child finished", nil
}

// fanoutExec simulates a fan-out parent's step executor: the first step is the
// turn that spawns (real progress), and every later step reports ErrWaiting until
// all children have converged, then folds.
type fanoutExec struct {
	t        *testing.T
	h        *harness
	children []reconcile.Ref
	calls    int
}

func (e *fanoutExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	e.calls++
	if e.calls == 1 {
		return json.RawMessage(`{"turn":1}`), nil
	}
	for _, c := range e.children {
		if e.h.status(e.t, c).Phase != PhaseConverged {
			return nil, ErrWaiting
		}
	}
	return json.RawMessage(`{"folded":true}`), nil
}

// recordingBus captures Publish calls so a test can assert the wake signal.
type recordingBus struct {
	mu   sync.Mutex
	msgs []bus.Message
}

func (b *recordingBus) Publish(_ context.Context, m bus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return nil
}

func (b *recordingBus) Subscribe(context.Context, string, bus.Handler) (bus.Subscription, error) {
	return nil, nil
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) published(subject string) []bus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []bus.Message
	for _, m := range b.msgs {
		if m.Subject == subject {
			out = append(out, m)
		}
	}
	return out
}

// createChild creates a child goal controller-owned by the parent, as the spawner
// does for a fan-out.
func createChild(t *testing.T, h *harness, name string, parent reconcile.Ref) reconcile.Ref {
	t.Helper()
	p, err := h.store.Get(h.ctx, parent.Kind, parent.Scope, parent.Name)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	raw, _ := json.Marshal(Spec{Objective: name, StopCondition: "done", Depth: 1})
	child := resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: name, Spec: raw}
	child.OwnerReferences = []resource.OwnerReference{{
		APIVersion: GroupVersion, Kind: Kind, Name: p.Name, ID: p.ID, Controller: true,
	}}
	if _, err := h.store.Put(h.ctx, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	return reconcile.Ref{Kind: Kind, Name: name}
}

// TestFanoutWaitParksWithoutBurningBudget is the gate for the fan-out busy-wait
// fix: a parent waiting on children runs re-check steps paced by child
// completions, not by the poll interval, and the wait neither consumes the step
// budget nor stalls the goal however long the children take.
func TestFanoutWaitParksWithoutBurningBudget(t *testing.T) {
	stop := &fanoutStop{childDone: map[string]bool{}}
	wake := &recordingBus{}
	h := newHarness(t, stop, WithWaitRecheck(time.Hour), WithWakeBus(wake))
	exec := &fanoutExec{t: t, h: h}
	w := NewWorker(h.store, h.jobs, h.clk, exec)

	parent := h.createGoal(t, "parent", Spec{Objective: "parent", StopCondition: "folded"})

	// Step 1: the turn that fans out (real progress).
	h.reconcile(t, parent)
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("fan-out step did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	if st := h.status(t, parent); st.Steps != 1 {
		t.Fatalf("after the fan-out turn Steps = %d, want 1", st.Steps)
	}

	// The children spawned by that turn, still running.
	exec.children = []reconcile.Ref{
		createChild(t, h, "child-1", parent),
		createChild(t, h, "child-2", parent),
		createChild(t, h, "child-3", parent),
	}

	// Step 2 was dispatched by the reconcile above; it finds the children running
	// and parks the parent.
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("wait check did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	st := h.status(t, parent)
	if st.Steps != 1 {
		t.Fatalf("the waiting check consumed the budget: Steps = %d, want 1", st.Steps)
	}
	if st.WaitingSince == nil {
		t.Fatal("parent is not marked waiting after the check")
	}
	if string(st.Checkpoint) != `{"turn":1}` {
		t.Fatalf("waiting check rewrote the checkpoint: %s", st.Checkpoint)
	}

	// The children outlast many poll cycles. The parked parent must dispatch no
	// jobs, take no steps, and never stall, however many cycles pass.
	for range 20 {
		h.clk.Advance(DefaultPollInterval)
		res := h.reconcile(t, parent)
		if res.RequeueAfter <= 0 {
			t.Fatal("parked parent stopped asking to be re-checked")
		}
		if ok, _ := w.ProcessOnce(h.ctx); ok {
			t.Fatal("parked parent dispatched a re-check job during the wait")
		}
	}
	st = h.status(t, parent)
	if st.Steps != 1 || st.Phase == PhaseStalled {
		t.Fatalf("the wait burned budget or stalled: Steps=%d Phase=%s", st.Steps, st.Phase)
	}
	if got := exec.calls; got != 2 {
		t.Fatalf("executor ran %d times during the wait, want 2", got)
	}

	// A child settles: its terminal reconcile clears the parent's park and signals.
	stop.childDone["child-1"] = true
	h.reconcile(t, exec.children[0])
	if h.status(t, exec.children[0]).Phase != PhaseConverged {
		t.Fatal("child-1 did not converge")
	}
	if st = h.status(t, parent); st.WaitingSince != nil {
		t.Fatal("child settling did not clear the parent's park")
	}
	if len(wake.published(StepSubject)) == 0 {
		t.Fatal("child settling published no wake signal")
	}

	// The woken parent re-checks once, finds two children still running, parks again.
	h.reconcile(t, parent)
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("woken parent dispatched no re-check (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	if st = h.status(t, parent); st.Steps != 1 || st.WaitingSince == nil {
		t.Fatalf("re-check after a wake miscounted: Steps=%d waiting=%v", st.Steps, st.WaitingSince)
	}

	// The remaining children settle; the parent folds and converges.
	stop.childDone["child-2"] = true
	stop.childDone["child-3"] = true
	h.reconcile(t, exec.children[1])
	h.reconcile(t, exec.children[2])
	h.reconcile(t, parent)
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("fold step did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)

	st = h.status(t, parent)
	if st.Phase != PhaseConverged {
		t.Fatalf("parent did not converge: %+v", st)
	}
	if st.Steps != 2 {
		t.Fatalf("parent Steps = %d, want 2 (turn + fold)", st.Steps)
	}
	// The whole wait cost one re-check per wake plus the turn and the fold:
	// O(child state-changes), not O(poll cycles).
	if exec.calls != 4 {
		t.Fatalf("executor ran %d times, want 4 (turn, first check, wake check, fold)", exec.calls)
	}
}

// TestFanoutWaitRecheckFallback covers a lost wake: a parked parent is re-checked
// after the recheck fallback elapses, and repeated fallback re-checks still do not
// consume the step budget.
func TestFanoutWaitRecheckFallback(t *testing.T) {
	stop := &fanoutStop{childDone: map[string]bool{}}
	recheck := time.Minute
	h := newHarness(t, stop, WithWaitRecheck(recheck))
	exec := &fanoutExec{t: t, h: h}
	w := NewWorker(h.store, h.jobs, h.clk, exec)

	parent := h.createGoal(t, "parent", Spec{Objective: "parent", StopCondition: "folded"})
	h.reconcile(t, parent)
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("fan-out step did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	exec.children = []reconcile.Ref{createChild(t, h, "child-1", parent)}
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("wait check did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	if st := h.status(t, parent); st.WaitingSince == nil {
		t.Fatal("parent is not parked")
	}

	// No wake arrives. Each elapsed fallback window buys exactly one re-check.
	for i := range 3 {
		h.clk.Advance(recheck + time.Second)
		h.reconcile(t, parent)
		if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
			t.Fatalf("fallback re-check %d did not run (ok=%v err=%v)", i, ok, err)
		}
		h.reconcile(t, parent)
		st := h.status(t, parent)
		if st.Steps != 1 || st.Phase == PhaseStalled {
			t.Fatalf("fallback re-check %d burned budget or stalled: Steps=%d Phase=%s", i, st.Steps, st.Phase)
		}
		if st.WaitingSince == nil {
			t.Fatalf("fallback re-check %d did not re-park", i)
		}
	}
	if exec.calls != 5 {
		t.Fatalf("executor ran %d times, want 5 (turn, first check, 3 fallbacks)", exec.calls)
	}

	// The child finally settles; the wake-less path still folds via the fallback.
	stop.childDone["child-1"] = true
	h.reconcile(t, exec.children[0])
	h.reconcile(t, parent)
	if ok, err := w.ProcessOnce(h.ctx); err != nil || !ok {
		t.Fatalf("fold step did not run (ok=%v err=%v)", ok, err)
	}
	h.reconcile(t, parent)
	if st := h.status(t, parent); st.Phase != PhaseConverged || st.Steps != 2 {
		t.Fatalf("parent did not fold and converge: %+v", st)
	}
}
