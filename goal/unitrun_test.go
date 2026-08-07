package goal

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// fakeSpawner is a UnitSpawner a test drives by hand: it records what it was asked to
// create, honours the idempotency the interface requires, and reports whatever the
// test says each child produced.
type fakeSpawner struct {
	mu sync.Mutex
	// spawned is the unit ids Spawn was called for, in order and including the calls
	// idempotency absorbed, so a test can prove a resumed run did not create a second
	// child rather than only that it did not record one.
	spawned  []string
	children map[string]string      // unit id -> child id
	outcomes map[string]UnitOutcome // child id -> what it produced
	refuse   map[string]error       // unit id -> the refusal Spawn returns
	pollErr  error                  // when set, Outcomes fails instead of reporting
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{
		children: map[string]string{},
		outcomes: map[string]UnitOutcome{},
		refuse:   map[string]error{},
	}
}

func (f *fakeSpawner) Spawn(_ context.Context, _ resource.Resource, u Unit) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned = append(f.spawned, u.ID)
	if id, ok := f.children[u.ID]; ok {
		return id, nil // idempotent: the same unit gets the same child
	}
	if err := f.refuse[u.ID]; err != nil {
		return "", err
	}
	id := "child-" + u.ID
	f.children[u.ID] = id
	return id, nil
}

func (f *fakeSpawner) Outcomes(_ context.Context, ids []string) ([]UnitOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	out := make([]UnitOutcome, 0, len(ids))
	for _, id := range ids {
		if o, ok := f.outcomes[id]; ok {
			out = append(out, o)
			continue
		}
		out = append(out, UnitOutcome{ChildID: id}) // still running
	}
	return out, nil
}

// finish tells the spawner what a unit's child produced. Evidence proves the unit;
// an empty evidence reference with a failure is a child that finished without it.
func (f *fakeSpawner) finish(unitID string, o UnitOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o.ChildID, o.Done = f.children[unitID], true
	f.outcomes[o.ChildID] = o
}

func (f *fakeSpawner) prove(unitID string) {
	f.finish(unitID, UnitOutcome{Evidence: "spine:" + unitID})
}

func (f *fakeSpawner) spawnedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spawned...)
}

// unitHarness is the reconciler harness with a fan-out wired and the wake a settling
// child would deliver made explicit, so a test drives the loop the way the runtime
// does rather than by advancing a clock past the fallback.
type unitHarness struct {
	*harness
	sp *fakeSpawner
}

func newUnitHarness(t *testing.T, stop StopEvaluator, opts ...Option) *unitHarness {
	t.Helper()
	sp := newFakeSpawner()
	h := newHarness(t, stop, append(opts, WithUnitSpawner(sp))...)
	return &unitHarness{harness: h, sp: sp}
}

// wake clears the parent's park the way a settling child does, so the next reconcile
// looks at the fan-out instead of short-circuiting on the park.
func (h *unitHarness) wake(t *testing.T, ref reconcile.Ref) {
	t.Helper()
	st := h.status(t, ref)
	st.WaitingSince = nil
	h.putStatus(t, ref, st)
}

// A graph runs its units as children in dependency order: a unit is created only
// once every unit it depends on has been proven, and the parent parks in between.
func TestUnitGraphRunsInDependencyOrder(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a"), unit("b", "a"), unit("c", "a"), unit("d", "b", "c")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer

	res := h.reconcile(t, ref)
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a"}) {
		t.Fatalf("first pass created %v, want [a]", got)
	}
	if res.RequeueAfter != h.gr.recheckAfter() {
		t.Fatalf("a parked parent requeued after %v, want the recheck fallback %v", res.RequeueAfter, h.gr.recheckAfter())
	}
	st := h.status(t, ref)
	if st.WaitingSince == nil || st.Phase != PhaseRunning || !hasCond(st, CondReconciling, "True") {
		t.Fatalf("the parent did not park on its children: %+v", st)
	}
	if st.Units[0].Phase != UnitDispatched || st.Units[0].ChildID != "child-a" {
		t.Fatalf("unit a was not recorded as dispatched: %+v", st.Units[0])
	}
	if claimed, _ := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)}); len(claimed) != 0 {
		t.Fatalf("a goal running its graph dispatched a %q step", claimed[0].Kind)
	}

	// A parked parent does not look at its children until something wakes it.
	h.reconcile(t, ref)
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a"}) {
		t.Fatalf("a parked parent created %v, want no new children", got)
	}

	h.sp.prove("a")
	h.wake(t, ref)
	h.reconcile(t, ref)
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a", "b", "c"}) {
		t.Fatalf("after a was proven the parent created %v, want b and c", got)
	}

	// d waits for both of its dependencies, not the first to land.
	h.sp.prove("b")
	h.wake(t, ref)
	h.reconcile(t, ref)
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a", "b", "c"}) {
		t.Fatalf("d was created with c still running: %v", got)
	}

	h.sp.prove("c")
	h.wake(t, ref)
	h.reconcile(t, ref)
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("d was not created once b and c were proven: %v", got)
	}

	h.sp.prove("d")
	h.wake(t, ref)
	h.reconcile(t, ref)
	st = h.status(t, ref)
	if !st.UnitsSettled(units) {
		t.Fatalf("the graph did not settle: %+v", st.Units)
	}
	if st.Phase != PhaseConverged {
		t.Fatalf("with the graph settled the goal did not go on to converge: %+v", st)
	}
}

// A child that finishes without a verification does not unblock anything. The graph
// stops, naming the unit that failed and what it stranded, rather than carrying an
// unproven result down a dependency edge.
func TestUnitWithoutEvidenceStopsTheGraph(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a"), unit("b", "a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // creates a, parks

	h.sp.finish("a", UnitOutcome{}) // done, no evidence, no explanation
	h.wake(t, ref)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
		t.Fatalf("a graph that can no longer proceed did not stall the goal: %+v", st)
	}
	if !strings.Contains(st.Message, noVerificationFailure) {
		t.Fatalf("stall message %q does not say the child proved nothing", st.Message)
	}
	if !strings.Contains(st.Message, "b blocked on a") {
		t.Fatalf("stall message %q does not name what the failure stranded", st.Message)
	}
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a"}) {
		t.Fatalf("b was created behind an unproven dependency: %v", got)
	}
	if st.WaitingSince != nil {
		t.Fatalf("a stalled goal is still parked: %+v", st)
	}
}

// A child that failed and said why keeps its own account of it on the record.
func TestFailedChildKeepsItsReason(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref)
	h.reconcile(t, ref)

	h.sp.finish("a", UnitOutcome{Failure: "the build never compiled"})
	h.wake(t, ref)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Units[0].Failure != "the build never compiled" {
		t.Fatalf("the unit lost the child's reason: %+v", st.Units[0])
	}
	if !strings.Contains(st.Message, "the build never compiled") {
		t.Fatalf("stall message %q does not carry the child's reason", st.Message)
	}
}

// The stop condition is not consulted while units are outstanding. A model that says
// the work is done cannot outrank a graph with unproven units in it, and the gate is
// the ordering rather than a check: the reconcile returns before the evaluator runs.
func TestGoalCannotConvergeWithUnitsOutstanding(t *testing.T) {
	stop := &countingStop{met: true}
	h := newUnitHarness(t, stop)
	units := []Unit{unit("a"), unit("b", "a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // creates a, parks

	if st := h.status(t, ref); st.Phase == PhaseConverged {
		t.Fatalf("a goal converged with its graph unstarted: %+v", st)
	}
	if stop.calls != 0 {
		t.Fatalf("the stop evaluator was consulted %d time(s) over an unsettled graph", stop.calls)
	}

	h.sp.prove("a")
	h.wake(t, ref)
	h.reconcile(t, ref) // creates b, parks
	if st := h.status(t, ref); st.Phase == PhaseConverged {
		t.Fatalf("a goal converged with a unit still running: %+v", st)
	}
	if stop.calls != 0 {
		t.Fatalf("the stop evaluator was consulted %d time(s) with a unit still running", stop.calls)
	}

	h.sp.prove("b")
	h.wake(t, ref)
	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("a goal with a settled graph did not converge: %+v", st)
	}
	if stop.calls == 0 {
		t.Fatalf("the stop evaluator was never consulted once the graph settled")
	}
}

// A spawn refused for good is the unit's outcome, not an error to retry: the unit
// settles unproven and the graph stops naming the refusal.
func TestSpawnRefusalSettlesTheUnit(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a"), unit("b", "a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.sp.refuse["a"] = fault.New(fault.Forbidden, "spawn_max_depth", "delegation depth exceeded")
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled {
		t.Fatalf("a refused spawn did not stall the goal: %+v", st)
	}
	if st.Units[0].Phase != UnitSettled || st.Units[0].Proven {
		t.Fatalf("a refused unit was not settled unproven: %+v", st.Units[0])
	}
	if !strings.Contains(st.Message, "delegation depth exceeded") {
		t.Fatalf("stall message %q does not name the refusal", st.Message)
	}
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a"}) {
		t.Fatalf("a refusal that will not come good was retried: %v", got)
	}
}

// A refusal that would succeed later is retried, and it does not cost the units
// admitted alongside it their record.
func TestTransientSpawnRefusalIsRetried(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	// Two roots, so the pass admits one before the other is refused.
	units := []Unit{unit("a"), unit("b")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.sp.refuse["b"] = fault.New(fault.Transient, "spawn_at_capacity", "fan-out is at capacity")

	// The first pass stamps the finalizer and goes straight on to the fan-out.
	if _, err := h.gr.Reconcile(h.ctx, ref); err == nil {
		t.Fatalf("a transient refusal was swallowed instead of retried")
	}
	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("a transient refusal stalled the goal: %q", st.Message)
	}
	if st.Units[0].ChildID != "child-a" {
		t.Fatalf("the unit admitted before the refusal lost its record: %+v", st.Units[0])
	}
	if st.Units[1].Phase != UnitPending {
		t.Fatalf("a transiently refused unit did not stay pending: %+v", st.Units[1])
	}

	delete(h.sp.refuse, "b")
	h.reconcile(t, ref)
	if got := h.status(t, ref).Units[1].ChildID; got != "child-b" {
		t.Fatalf("the retried unit was not created: %q", got)
	}
	if got := h.sp.spawnedIDs(); !equalIDs(got, []string{"a", "b", "b"}) {
		t.Fatalf("spawn calls = %v, want a, the refused b, then the retried b", got)
	}
}

// The resume case. A crash between creating a child and recording it re-enters
// Spawn, and because Spawn is idempotent the run is handed the same child back: the
// fan-out resumes with the children it had rather than a second set of them.
func TestUnitDispatchResumesWithoutDoubleSpawning(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a"), unit("b")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer

	before := h.status(t, ref)
	h.reconcile(t, ref) // creates both children and records them
	after := h.status(t, ref)
	if after.Units[0].ChildID == "" || after.Units[1].ChildID == "" {
		t.Fatalf("the pass did not record both children: %+v", after.Units)
	}

	// Play the crash: the children exist, the write that recorded them did not land.
	h.putStatus(t, ref, before)
	h.reconcile(t, ref)

	resumed := h.status(t, ref)
	for i := range resumed.Units {
		if resumed.Units[i].ChildID != after.Units[i].ChildID {
			t.Fatalf("unit %s resumed onto a different child: %q then %q",
				resumed.Units[i].ID, after.Units[i].ChildID, resumed.Units[i].ChildID)
		}
		if resumed.Units[i].Phase != UnitDispatched {
			t.Fatalf("unit %s did not resume dispatched: %+v", resumed.Units[i].ID, resumed.Units[i])
		}
	}
	if got := len(h.sp.children); got != 2 {
		t.Fatalf("the resumed run left %d children, want 2", got)
	}
}

// A goal carrying a graph with nothing wired to run it stalls. A graph that is
// admitted, validated and then quietly ignored is the loaded-gate-that-does-nothing
// failure, and it would read to a caller as a goal that simply never fanned out.
func TestGoalWithAUnitGraphAndNoSpawnerStalls(t *testing.T) {
	h := newHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: []Unit{unit("a")}})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
		t.Fatalf("a graph with no spawner did not stall the goal: %+v", st)
	}
	if !strings.Contains(st.Message, "no spawner is wired") {
		t.Fatalf("stall message %q does not say what is missing", st.Message)
	}
}

// A goal with no graph never enters any of this, spawner wired or not.
func TestUnitLoopIsInertWithoutAGraph(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", MaxSteps: 3})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // step 1
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("a goal with no graph did not converge: %+v", st)
	}
	if len(h.sp.spawnedIDs()) != 0 {
		t.Fatalf("a goal with no graph created children: %v", h.sp.spawnedIDs())
	}
	if st.Units != nil {
		t.Fatalf("a goal with no graph recorded unit state: %+v", st.Units)
	}
}

// The fan-out's behavioural contract under chaos: for any acyclic graph, with
// children settling in any order and spurious reconciles injected, the goal reaches a
// terminal phase, no unit is ever started before every unit it depends on is proven,
// and no unit ever gets a second child.
func TestUnitFanoutProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(rt, "units")
		units := make([]Unit, 0, n)
		for i := range n {
			var deps []string
			for j := range i {
				if rapid.Bool().Draw(rt, fmt.Sprintf("edge-%d-%d", j, i)) {
					deps = append(deps, fmt.Sprintf("u%d", j))
				}
			}
			units = append(units, unit(fmt.Sprintf("u%d", i), deps...))
		}
		for i := len(units) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(rt, fmt.Sprintf("shuffle-%d", i))
			units[i], units[j] = units[j], units[i]
		}

		h := newUnitHarness(t, stopAfter{at: 0})
		ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
		seen := map[string]bool{}
		for range 100 {
			h.reconcile(t, ref)
			// Chaos: a spurious re-trigger must never create a duplicate child.
			if rapid.Bool().Draw(rt, "spurious") {
				h.reconcile(t, ref)
			}

			st := h.status(t, ref)
			for _, u := range st.Units {
				if u.Phase == UnitPending || seen[u.ID] {
					continue
				}
				seen[u.ID] = true
				if unmet := st.UnmetDependencies(units, u.ID); len(unmet) > 0 {
					rt.Fatalf("%s was started with %v unproven", u.ID, unmet)
				}
			}
			if st.Phase == PhaseConverged || st.Phase == PhaseStalled {
				if st.Phase != PhaseConverged {
					rt.Fatalf("a graph whose children all succeed stalled: %q", st.Message)
				}
				if !st.UnitsSettled(units) {
					rt.Fatalf("the goal converged over an unsettled graph: %+v", st.Units)
				}
				if got := len(h.sp.children); got != n {
					rt.Fatalf("%d children for %d units", got, n)
				}
				return
			}
			// Settle one running child, in whatever order the draw picks.
			if running := st.DispatchedUnits(); len(running) > 0 {
				pick := running[rapid.IntRange(0, len(running)-1).Draw(rt, "settle")]
				h.sp.prove(strings.TrimPrefix(pick, "child-"))
			}
			h.wake(t, ref)
		}
		rt.Fatal("the fan-out did not reach a terminal phase")
	})
}

// A fan-out that cannot be polled is a read that failed, not a graph that stopped.
// The reconcile returns the error to be classified and retried, and the goal keeps
// its children rather than settling over a question nobody answered.
func TestOutcomesErrorIsRetriedRatherThanStalling(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 0})
	units := []Unit{unit("a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer, creates a's child and parks

	h.sp.pollErr = fault.New(fault.Transient, "spawn_poll_get", "the store is unreachable")
	h.wake(t, ref)
	if _, err := h.gr.Reconcile(h.ctx, ref); err == nil {
		t.Fatalf("a failed poll was swallowed")
	}
	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("a failed poll stalled the goal: %q", st.Message)
	}
	if st.Units[0].Phase != UnitDispatched {
		t.Fatalf("a failed poll disturbed the unit's state: %+v", st.Units[0])
	}

	h.sp.pollErr = nil
	h.sp.prove("a")
	h.wake(t, ref)
	h.reconcile(t, ref)
	if st := h.status(t, ref); st.Phase != PhaseConverged {
		t.Fatalf("the goal did not recover once the poll came back: %+v", st)
	}
}

// countingStop reports a fixed verdict and counts how often it was asked, which is
// how the convergence gate is proven to be an ordering rather than a check: an
// evaluator that is never consulted cannot have been overridden.
type countingStop struct {
	met   bool
	calls int
}

func (s *countingStop) Met(context.Context, Spec, Status) (bool, string, error) {
	s.calls++
	return s.met, "stop condition met", nil
}

var _ StopEvaluator = (*countingStop)(nil)
