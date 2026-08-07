package goal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
)

// unit builds a unit with the given id and dependencies, filling the required text
// so a test that is about the graph does not have to restate it.
func unit(id string, deps ...string) Unit {
	return Unit{ID: id, Objective: "do " + id, Verify: "check " + id, DependsOn: deps}
}

// syncedStatus is a status that has already admitted the graph, which is the state
// every reader below expects to be handed.
func syncedStatus(units []Unit) Status {
	var s Status
	s.SyncUnits(units)
	return s
}

// dispatchAndProve runs a unit all the way through: a child is created for it and
// the child comes back having proven it.
func dispatchAndProve(t *testing.T, s *Status, u Unit) {
	t.Helper()
	if err := s.MarkUnitDispatched(u, "child-"+u.ID); err != nil {
		t.Fatalf("dispatch %s: %v", u.ID, err)
	}
	if err := s.MarkUnitProven(u.ID, "spine:"+u.ID, time.Unix(0, 0)); err != nil {
		t.Fatalf("prove %s: %v", u.ID, err)
	}
}

func TestValidateUnits(t *testing.T) {
	tests := []struct {
		name  string
		units []Unit
		want  error
	}{
		{name: "empty graph is valid"},
		{name: "single root", units: []Unit{unit("a")}},
		{name: "chain", units: []Unit{unit("a"), unit("b", "a"), unit("c", "b")}},
		{name: "diamond", units: []Unit{unit("a"), unit("b", "a"), unit("c", "a"), unit("d", "b", "c")}},
		{
			name:  "declared out of dependency order",
			units: []Unit{unit("d", "b", "c"), unit("b", "a"), unit("c", "a"), unit("a")},
		},
		{
			name:  "missing id",
			units: []Unit{{Objective: "o", Verify: "v"}},
			want:  ErrUnitIncomplete,
		},
		{
			name:  "missing objective",
			units: []Unit{{ID: "a", Verify: "v"}},
			want:  ErrUnitIncomplete,
		},
		{
			name:  "missing verify",
			units: []Unit{{ID: "a", Objective: "o"}},
			want:  ErrUnitIncomplete,
		},
		{
			name:  "whitespace is not content",
			units: []Unit{{ID: "a", Objective: "o", Verify: "   "}},
			want:  ErrUnitIncomplete,
		},
		{
			name:  "duplicate id",
			units: []Unit{unit("a"), unit("a")},
			want:  ErrUnitDuplicate,
		},
		{
			name:  "dangling dependency",
			units: []Unit{unit("a", "ghost")},
			want:  ErrUnitUnknownDependency,
		},
		{
			name:  "self dependency",
			units: []Unit{unit("a", "a")},
			want:  ErrUnitCycle,
		},
		{
			name:  "two-unit cycle",
			units: []Unit{unit("a", "b"), unit("b", "a")},
			want:  ErrUnitCycle,
		},
		{
			name:  "cycle with a healthy root",
			units: []Unit{unit("root"), unit("a", "b"), unit("b", "a")},
			want:  ErrUnitCycle,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUnits(tt.units)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidateUnits() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ValidateUnits() = %v, want %v", err, tt.want)
			}
		})
	}
}

// A cycle refusal names every unit the cycle strands, not just the units on it. The
// units downstream of a cycle are equally unrunnable, and whoever has to fix the
// graph is better served by the whole set than by one edge of it.
func TestValidateUnitsNamesTheWholeStrandedSet(t *testing.T) {
	units := []Unit{unit("ok"), unit("a", "b"), unit("b", "a"), unit("downstream", "a")}
	err := ValidateUnits(units)
	if !errors.Is(err, ErrUnitCycle) {
		t.Fatalf("ValidateUnits() = %v, want ErrUnitCycle", err)
	}
	for _, want := range []string{"a", "b", "downstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cycle error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "ok") {
		t.Errorf("cycle error %q names the unit that is fine", err)
	}
}

func TestUnitMarkDistinguishesEveryField(t *testing.T) {
	base := Unit{ID: "a", Objective: "o", Verify: "v", DependsOn: []string{"x"}, Actions: []string{"read"}, Agent: "scout"}
	variants := map[string]Unit{
		"id":         {ID: "b", Objective: "o", Verify: "v", DependsOn: []string{"x"}, Actions: []string{"read"}, Agent: "scout"},
		"objective":  {ID: "a", Objective: "other", Verify: "v", DependsOn: []string{"x"}, Actions: []string{"read"}, Agent: "scout"},
		"verify":     {ID: "a", Objective: "o", Verify: "other", DependsOn: []string{"x"}, Actions: []string{"read"}, Agent: "scout"},
		"dependency": {ID: "a", Objective: "o", Verify: "v", DependsOn: []string{"y"}, Actions: []string{"read"}, Agent: "scout"},
		"dep added":  {ID: "a", Objective: "o", Verify: "v", DependsOn: []string{"x", "y"}, Actions: []string{"read"}, Agent: "scout"},
		"dep gone":   {ID: "a", Objective: "o", Verify: "v", Actions: []string{"read"}, Agent: "scout"},
		"action":     {ID: "a", Objective: "o", Verify: "v", DependsOn: []string{"x"}, Actions: []string{"write"}, Agent: "scout"},
		"agent":      {ID: "a", Objective: "o", Verify: "v", DependsOn: []string{"x"}, Actions: []string{"read"}, Agent: "other"},
	}
	want := UnitMark(base)
	if got := UnitMark(base); got != want {
		t.Fatalf("UnitMark is not stable: %s then %s", want, got)
	}
	for name, v := range variants {
		if UnitMark(v) == want {
			t.Errorf("changing the %s left the mark unchanged (%s)", name, want)
		}
	}
}

// Two units whose fields concatenate to the same bytes must still fingerprint
// differently, or a rewrite could be dressed as the unit it replaced.
func TestUnitMarkCannotBeResplit(t *testing.T) {
	a := Unit{ID: "ab", Objective: "c", Verify: "v"}
	b := Unit{ID: "a", Objective: "bc", Verify: "v"}
	if UnitMark(a) == UnitMark(b) {
		t.Fatalf("units re-split to the same mark: %+v and %+v", a, b)
	}
}

func TestSyncUnitsCarriesStateAndDropsOrphans(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a")}
	s := syncedStatus(units)
	if len(s.Units) != 2 || s.Units[0].ID != "a" || s.Units[1].ID != "b" {
		t.Fatalf("sync did not create one pending entry per unit in order: %+v", s.Units)
	}
	for _, st := range s.Units {
		if st.Phase != UnitPending || st.Proven {
			t.Fatalf("unit %s did not start pending and unproven: %+v", st.ID, st)
		}
	}

	dispatchAndProve(t, &s, units[0])

	// A graph that grew gains state for the new unit without disturbing the old.
	grown := append(units, unit("c", "b"))
	s.SyncUnits(grown)
	if len(s.Units) != 3 {
		t.Fatalf("sync over a grown graph = %d entries, want 3", len(s.Units))
	}
	if !s.Units[0].Proven || s.Units[0].ChildID != "child-a" {
		t.Fatalf("sync lost the proof for a: %+v", s.Units[0])
	}
	if s.Units[2].Phase != UnitPending {
		t.Fatalf("the new unit did not arrive pending: %+v", s.Units[2])
	}

	// A pending unit removed from the graph takes its (empty) state with it.
	s.SyncUnits(units)
	if len(s.Units) != 2 {
		t.Fatalf("sync over a shrunk graph = %d entries, want 2", len(s.Units))
	}

	s.SyncUnits(nil)
	if s.Units != nil {
		t.Fatalf("sync over an empty graph left %+v", s.Units)
	}
}

func TestValidateDispatchedRefusesRewrites(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a")}
	s := syncedStatus(units)
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if err := s.ValidateDispatched(units); err != nil {
		t.Fatalf("unchanged graph refused: %v", err)
	}

	// A pending unit is not a commitment: it may be rewritten, removed or reordered.
	repointed := []Unit{unit("a"), {ID: "b", Objective: "different", Verify: "different"}}
	if err := s.ValidateDispatched(repointed); err != nil {
		t.Fatalf("rewriting a pending unit was refused: %v", err)
	}
	if err := s.ValidateDispatched([]Unit{unit("a")}); err != nil {
		t.Fatalf("dropping a pending unit was refused: %v", err)
	}

	rewritten := []Unit{{ID: "a", Objective: "something else", Verify: "check a"}, unit("b", "a")}
	if err := s.ValidateDispatched(rewritten); !errors.Is(err, ErrUnitRewritten) {
		t.Fatalf("rewriting a dispatched unit = %v, want ErrUnitRewritten", err)
	}
	// Re-pointing a dispatched unit's edges rewrites what the graph claims stood
	// proven before its child started, so it is refused too.
	repointedEdges := []Unit{unit("a", "b"), unit("b")}
	if err := s.ValidateDispatched(repointedEdges); !errors.Is(err, ErrUnitRewritten) {
		t.Fatalf("re-pointing a dispatched unit = %v, want ErrUnitRewritten", err)
	}
	if err := s.ValidateDispatched([]Unit{unit("b", "a")}); !errors.Is(err, ErrUnitRewritten) {
		t.Fatalf("dropping a dispatched unit = %v, want ErrUnitRewritten", err)
	}
}

func TestReadyUnitsAdmitsInDependencyOrder(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a"), unit("c", "a"), unit("d", "b", "c")}
	s := syncedStatus(units)

	if got := unitIDs(s.ReadyUnits(units)); !equalIDs(got, []string{"a"}) {
		t.Fatalf("ready at the start = %v, want [a]", got)
	}
	if s.UnitPhaseOf(units, "d") != UnitBlocked {
		t.Fatalf("d is not blocked before its dependencies are proven")
	}
	if got := s.UnmetDependencies(units, "d"); !equalIDs(got, []string{"b", "c"}) {
		t.Fatalf("d is waiting on %v, want [b c]", got)
	}

	// A dispatched unit is not ready again: that is what stops a resumed run doubling
	// a fan-out.
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch a: %v", err)
	}
	if got := s.ReadyUnits(units); len(got) != 0 {
		t.Fatalf("ready while a is in flight = %v, want none", unitIDs(got))
	}
	if s.UnitPhaseOf(units, "a") != UnitDispatched {
		t.Fatalf("a is not dispatched: %+v", s.Units[0])
	}

	if err := s.MarkUnitProven("a", "spine:a", time.Unix(0, 0)); err != nil {
		t.Fatalf("prove a: %v", err)
	}
	if got := unitIDs(s.ReadyUnits(units)); !equalIDs(got, []string{"b", "c"}) {
		t.Fatalf("ready after a = %v, want [b c]", got)
	}

	dispatchAndProve(t, &s, units[1])
	if got := unitIDs(s.ReadyUnits(units)); !equalIDs(got, []string{"c"}) {
		t.Fatalf("ready after b = %v, want [c] (d still waits on c)", got)
	}
	dispatchAndProve(t, &s, units[2])
	if got := unitIDs(s.ReadyUnits(units)); !equalIDs(got, []string{"d"}) {
		t.Fatalf("ready after c = %v, want [d]", got)
	}
	dispatchAndProve(t, &s, units[3])
	if !s.UnitsSettled(units) {
		t.Fatalf("graph did not settle: %+v", s.Units)
	}
}

// A unit that settles without proving anything never unblocks its dependents. This
// is the invariant that keeps a dependency edge meaning "proven", rather than
// decaying into "the child exited".
func TestSettledWithoutProofDoesNotUnblock(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a")}
	s := syncedStatus(units)
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch a: %v", err)
	}
	if err := s.MarkUnitFailed("a", "the check did not pass", time.Unix(0, 0)); err != nil {
		t.Fatalf("fail a: %v", err)
	}
	if got := s.ReadyUnits(units); len(got) != 0 {
		t.Fatalf("b became ready behind a failed dependency: %v", unitIDs(got))
	}
	if s.UnitPhaseOf(units, "b") != UnitBlocked {
		t.Fatalf("b is not blocked behind a failed dependency")
	}
	if s.UnitsSettled(units) {
		t.Fatalf("a graph with a failed unit reported settled")
	}
	stalled, reason := s.UnitStalled(units)
	if !stalled {
		t.Fatalf("a graph that can no longer proceed did not report stalled")
	}
	if !strings.Contains(reason, "a failed") || !strings.Contains(reason, "b blocked on a") {
		t.Fatalf("stall reason %q does not name the failure and what it stranded", reason)
	}
}

func TestUnitStalledOnlyWhenNothingCanProceed(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a")}
	s := syncedStatus(units)
	if stalled, _ := s.UnitStalled(units); stalled {
		t.Fatalf("a graph with a ready unit reported stalled")
	}
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch a: %v", err)
	}
	if stalled, _ := s.UnitStalled(units); stalled {
		t.Fatalf("a graph with a child in flight reported stalled")
	}
	if err := s.MarkUnitProven("a", "spine:a", time.Unix(0, 0)); err != nil {
		t.Fatalf("prove a: %v", err)
	}
	dispatchAndProve(t, &s, units[1])
	if stalled, _ := s.UnitStalled(units); stalled {
		t.Fatalf("a settled graph reported stalled")
	}
	if stalled, _ := (Status{}).UnitStalled(nil); stalled {
		t.Fatalf("a goal with no graph reported stalled")
	}
}

// A settled unit that produced no verification still says something legible about
// why the graph stopped, rather than trailing off after the unit id.
func TestUnitStallNamesASilentFailure(t *testing.T) {
	units := []Unit{unit("a"), unit("b", "a")}
	s := syncedStatus(units)
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch a: %v", err)
	}
	if err := s.MarkUnitFailed("a", "", time.Unix(0, 0)); err != nil {
		t.Fatalf("fail a: %v", err)
	}
	_, reason := s.UnitStalled(units)
	if !strings.Contains(reason, "no verification was recorded") {
		t.Fatalf("stall reason %q does not account for a silent failure", reason)
	}
}

func TestMarkUnitDispatchedRefusesASecondChild(t *testing.T) {
	units := []Unit{unit("a")}
	s := syncedStatus(units)
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkUnitDispatched(units[0], "child-a-again"); err == nil {
		t.Fatalf("a second dispatch was accepted")
	}
	if s.Units[0].ChildID != "child-a" {
		t.Fatalf("the second dispatch overwrote the first child: %+v", s.Units[0])
	}
	if s.Units[0].Mark != UnitMark(units[0]) {
		t.Fatalf("dispatch did not stamp the unit's mark: %+v", s.Units[0])
	}
	if err := s.MarkUnitDispatched(unit("ghost"), "child-ghost"); !errors.Is(err, ErrUnitUnknown) {
		t.Fatalf("dispatching a unit outside the graph = %v, want ErrUnitUnknown", err)
	}
}

func TestSettleRequiresADispatch(t *testing.T) {
	units := []Unit{unit("a")}
	s := syncedStatus(units)
	if err := s.MarkUnitProven("a", "spine:a", time.Unix(0, 0)); !errors.Is(err, ErrUnitNotDispatched) {
		t.Fatalf("proving an undispatched unit = %v, want ErrUnitNotDispatched", err)
	}
	if err := s.MarkUnitFailed("a", "nope", time.Unix(0, 0)); !errors.Is(err, ErrUnitNotDispatched) {
		t.Fatalf("failing an undispatched unit = %v, want ErrUnitNotDispatched", err)
	}
	if err := s.MarkUnitProven("ghost", "spine:x", time.Unix(0, 0)); !errors.Is(err, ErrUnitUnknown) {
		t.Fatalf("proving a unit outside the graph = %v, want ErrUnitUnknown", err)
	}
	if s.Units[0].Phase != UnitPending {
		t.Fatalf("a refused settle still moved the unit: %+v", s.Units[0])
	}
}

func TestSettleKeepsTheFirstOutcome(t *testing.T) {
	units := []Unit{unit("a")}
	s := syncedStatus(units)
	first := time.Unix(100, 0)
	if err := s.MarkUnitDispatched(units[0], "child-a"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := s.MarkUnitProven("a", "spine:first", first); err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := s.MarkUnitProven("a", "spine:second", time.Unix(200, 0)); err != nil {
		t.Fatalf("re-prove: %v", err)
	}
	if s.Units[0].Evidence != "spine:first" || !s.Units[0].SettledAt.Equal(first) {
		t.Fatalf("re-proving restamped the outcome: %+v", s.Units[0])
	}
	// A failure arriving after a proof must not unprove settled work.
	if err := s.MarkUnitFailed("a", "late failure", time.Unix(300, 0)); err != nil {
		t.Fatalf("late failure: %v", err)
	}
	if !s.Units[0].Proven || s.Units[0].Failure != "" {
		t.Fatalf("a late failure overwrote a proof: %+v", s.Units[0])
	}
}

// The unit graph is a projection of the record, so every reader has to agree with
// every other. A run that dispatches whatever is ready, one unit at a time, must
// settle any acyclic graph, dispatch each unit exactly once, and never start a unit
// before every dependency is proven, whatever order the units are written in.
func TestUnitAdmissionOrderProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "units")
		// Depending only on lower-numbered units makes the graph acyclic by
		// construction; the shuffle below then removes any help the declaration order
		// would otherwise give the reader.
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

		if err := ValidateUnits(units); err != nil {
			rt.Fatalf("an acyclic graph was refused: %v", err)
		}
		byID := make(map[string]Unit, len(units))
		for _, u := range units {
			byID[u.ID] = u
		}

		s := syncedStatus(units)
		dispatched := make(map[string]bool, len(units))
		for range 2 * len(units) {
			if s.UnitsSettled(units) {
				break
			}
			ready := s.ReadyUnits(units)
			if len(ready) == 0 {
				rt.Fatalf("nothing ready over an acyclic graph: %+v", s.Units)
			}
			u := ready[rapid.IntRange(0, len(ready)-1).Draw(rt, "pick")]
			for _, d := range byID[u.ID].DependsOn {
				st, _ := s.unitState(d)
				if !st.Proven {
					rt.Fatalf("%s was admitted with %s unproven", u.ID, d)
				}
			}
			if dispatched[u.ID] {
				rt.Fatalf("%s was dispatched twice", u.ID)
			}
			dispatched[u.ID] = true
			dispatchAndProve(t, &s, u)
			if err := s.ValidateDispatched(units); err != nil {
				rt.Fatalf("the graph refused itself mid-run: %v", err)
			}
		}
		if !s.UnitsSettled(units) {
			rt.Fatalf("graph did not settle in %d rounds: %+v", 2*len(units), s.Units)
		}
		if len(dispatched) != len(units) {
			rt.Fatalf("dispatched %d of %d units", len(dispatched), len(units))
		}
	})
}

// --- admission on the reconcile path ----------------------------------------

// putStatus writes a status onto the goal, so a test can stand the record up in a
// state the reconciler would take several passes to reach.
func (h *harness) putStatus(t *testing.T, ref reconcile.Ref, st Status) {
	t.Helper()
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	enc, err := st.Encode()
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	r.Status = enc
	if _, err := h.store.Put(h.ctx, r); err != nil {
		t.Fatalf("put status: %v", err)
	}
}

// putSpec rewrites the goal's spec, which is how a test plays the edit the graph's
// no-rewrite rule exists to refuse.
func (h *harness) putSpec(t *testing.T, ref reconcile.Ref, spec Spec) {
	t.Helper()
	r, err := h.store.Get(h.ctx, ref.Kind, ref.Scope, ref.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	r.Spec = raw
	if _, err := h.store.Put(h.ctx, r); err != nil {
		t.Fatalf("put spec: %v", err)
	}
}

// A graph that could never run is a terminal spec fault, settled before any child
// is created. Discovering it later means children are already running against a
// plan that was never runnable.
func TestGoalRefusesAnUnrunnableUnitGraph(t *testing.T) {
	tests := []struct {
		name  string
		units []Unit
		want  string
	}{
		{name: "cycle", units: []Unit{unit("a", "b"), unit("b", "a")}, want: "dependency cycle"},
		{name: "dangling edge", units: []Unit{unit("a", "ghost")}, want: "unknown unit"},
		{name: "duplicate id", units: []Unit{unit("a"), unit("a")}, want: "duplicate unit id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, stopAfter{at: 1})
			ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: tt.units})
			h.reconcile(t, ref) // finalizer
			h.reconcile(t, ref)

			st := h.status(t, ref)
			if st.Phase != PhaseStalled || !hasCond(st, CondStalled, "True") {
				t.Fatalf("an unrunnable graph did not stall the goal: %+v", st)
			}
			if !strings.Contains(st.Message, tt.want) {
				t.Fatalf("stall message %q does not name the fault (%s)", st.Message, tt.want)
			}
			claimed, err := h.jobs.Claim(h.ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(time.Minute)})
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if len(claimed) != 0 {
				t.Fatalf("an unrunnable graph enqueued a %q job", claimed[0].Kind)
			}
		})
	}
}

// A unit whose child is already running may not be rewritten under it.
func TestGoalRefusesARewrittenUnitMidRun(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 99})
	units := []Unit{unit("a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer, and the pass that creates a's child

	if st := h.status(t, ref); st.Units[0].Phase != UnitDispatched {
		t.Fatalf("the unit under test is not in flight: %+v", st.Units[0])
	}
	h.putSpec(t, ref, Spec{
		Objective: "o", StopCondition: "c",
		Units: []Unit{{ID: "a", Objective: "something else", Verify: "check a"}},
	})

	h.reconcile(t, ref)
	got := h.status(t, ref)
	if got.Phase != PhaseStalled {
		t.Fatalf("a rewritten unit did not stall the goal: %+v", got)
	}
	if !strings.Contains(got.Message, "rewritten") {
		t.Fatalf("stall message %q does not name the rewrite", got.Message)
	}
}

// A goal that carries no graph is untouched by any of this: it runs, converges and
// records nothing about units.
func TestGoalWithoutAUnitGraphIsUnchanged(t *testing.T) {
	h := newHarness(t, stopAfter{at: 1})
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", MaxSteps: 3})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref) // step 1
	h.completeStep(t)
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase != PhaseConverged {
		t.Fatalf("a goal with no graph did not converge: %+v", st)
	}
	if st.Units != nil {
		t.Fatalf("a goal with no graph recorded unit state: %+v", st.Units)
	}
}

// Admitting a valid graph records one pending unit per node and otherwise leaves the
// run alone: the graph is desired state the reconciler has observed, not yet work it
// has done.
func TestGoalAdmitsAValidUnitGraph(t *testing.T) {
	h := newUnitHarness(t, stopAfter{at: 99})
	units := []Unit{unit("a"), unit("b", "a")}
	ref := h.createGoal(t, "g", Spec{Objective: "o", StopCondition: "c", Units: units})
	h.reconcile(t, ref) // finalizer
	h.reconcile(t, ref)

	st := h.status(t, ref)
	if st.Phase == PhaseStalled {
		t.Fatalf("a valid graph stalled the goal: %q", st.Message)
	}
	if len(st.Units) != 2 || st.Units[0].ID != "a" || st.Units[1].ID != "b" {
		t.Fatalf("admission did not record the graph: %+v", st.Units)
	}
	if st.Units[1].Phase != UnitPending || st.Units[1].ChildID != "" {
		t.Fatalf("a unit behind an unproven dependency was not left pending: %+v", st.Units[1])
	}
}

func unitIDs(units []Unit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.ID)
	}
	return out
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
