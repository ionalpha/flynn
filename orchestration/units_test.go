package orchestration_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
)

// governedUnits wires the fan-out through a real dispatch waist with the capability
// admitter on it, which is what a composition drives: none of this is a stand-in that
// lets a spawn past governance the model path would have been refused by.
func governedUnits(sp *orchestration.Spawner) *orchestration.UnitFanout {
	return orchestration.Units(sp, dispatch.New(dispatch.WithAdmitter(capability.Admitter{})))
}

// unit is the graph node the tests spawn from, with the two required fields filled and
// the rest at their defaults.
func unit(id string, actions ...string) goal.Unit {
	return goal.Unit{
		ID:        id,
		Objective: "do the " + id + " work",
		Verify:    "go test ./" + id,
		Actions:   actions,
	}
}

// settleLedger writes a finished child's phase and its ledger state, which is the
// record a unit is settled from.
func settleLedger(t *testing.T, s resource.Store, id string, phase goal.Phase, msg string, ledger []goal.LedgerState) {
	t.Helper()
	r, err := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := goal.Status{Phase: phase, Message: msg, Ledger: ledger}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r.Status = enc
	if _, err := s.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// proven is one ledger item settled with a verification behind it.
func proven(id, evidence string) goal.LedgerState {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return goal.LedgerState{ID: id, Proven: true, ProvenAt: &at, Evidence: evidence}
}

// A unit's child is a governed child like any other, plus the three things that make
// it a unit's: a name derived from the unit, the unit's verify clause as a ledger it is
// judged against, and its own stop condition rather than the parent's.
func TestUnitChildIsGovernedAndCarriesItsLedger(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)
	parent := putParent(t, s, "root", goal.Spec{
		Objective: "lead", StopCondition: "the whole plan is delivered",
		Grant: []string{"read", "write", "bash", "spawn", mission.ActionModelGenerate},
	})

	u := unit("parser", "read", "write")
	id, err := governedUnits(sp).Spawn(context.Background(), parent, u)
	if err != nil {
		t.Fatalf("spawn unit: %v", err)
	}
	if want := orchestration.UnitChildName("root", "parser"); id != want {
		t.Fatalf("child id = %q, want the derived %q", id, want)
	}
	if len(enq.keys) != 1 || enq.keys[0].Name != id {
		t.Fatalf("the unit's child was not enqueued: %v", enq.keys)
	}

	cs := childSpec(t, s, id)
	if cs.Depth != 1 {
		t.Fatalf("child depth = %d, want 1", cs.Depth)
	}
	if cs.BudgetPool != "root" {
		t.Fatalf("child pool = %q, want the parent's", cs.BudgetPool)
	}
	// The requested subset of the parent's authority, plus the model call every loop
	// needs to think at all, and nothing else: not bash, not the right to spawn again.
	sort.Strings(cs.Grant)
	if want := []string{mission.ActionModelGenerate, "read", "write"}; !slices.Equal(cs.Grant, want) {
		t.Fatalf("child grant = %v, want %v", cs.Grant, want)
	}
	if len(cs.Ledger) != 1 {
		t.Fatalf("child ledger = %+v, want the unit's one item", cs.Ledger)
	}
	if cs.Ledger[0].Verify != u.Verify || cs.Ledger[0].Item != u.Objective {
		t.Fatalf("the child's ledger item is not the unit's: %+v", cs.Ledger[0])
	}
	if want := goal.ItemID(u.Objective, u.Verify); cs.Ledger[0].ID != want {
		t.Fatalf("ledger item id = %q, want the content address %q", cs.Ledger[0].ID, want)
	}
	if !strings.Contains(cs.StopCondition, u.Verify) {
		t.Fatalf("child stop condition %q does not quote the unit's check", cs.StopCondition)
	}
	if cs.StopCondition == "the whole plan is delivered" {
		t.Fatal("the child inherited its parent's completion condition")
	}

	// Handed a ledger, the child is already planned: a planning-enabled runtime must not
	// expand its objective into a second, different definition of done.
	r, err := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatal(err)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Planned {
		t.Fatalf("a child handed its ledger was not recorded as planned: %+v", status)
	}
}

// The idempotency the reconciler relies on: it records the child after the create
// returns, so the same unit spawned twice must be the same child, created and charged
// for once. The concurrency cap of one is the proof it was not created twice; a second
// create would be refused at capacity instead of returning.
func TestUnitSpawnIsIdempotentPerUnit(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	l := budget.NewLedger(s)
	if _, err := l.Open(context.Background(), "root", resource.Scope{}, budget.Limits{Tokens: 100}); err != nil {
		t.Fatal(err)
	}
	sp := orchestration.NewSpawner(s, l,
		orchestration.WithConcurrency(1), orchestration.WithReservation(budget.Spent{Tokens: 60}))
	sp.SetEnqueue(enq.fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})
	units := governedUnits(sp)

	first, err := units.Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	second, err := units.Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("the repeated spawn was refused instead of returning the same child: %v", err)
	}
	if first != second {
		t.Fatalf("the same unit got two children: %q then %q", first, second)
	}
	if len(enq.keys) != 1 {
		t.Fatalf("the repeated spawn enqueued again: %v", enq.keys)
	}
}

// The resume case, which is the one idempotency exists for: the process that created
// the children is gone and its bookkeeping with it, but the children are on the store.
// A fresh spawner is handed the same child back rather than creating a second one.
func TestUnitSpawnResumesOntoAnExistingChild(t *testing.T) {
	s := newStore(t)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})

	before := orchestration.NewSpawner(s, nil)
	before.SetEnqueue((&recordingEnqueue{}).fn)
	first, err := governedUnits(before).Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}

	// A restart: a new spawner over the same store, with nothing in memory.
	enq := &recordingEnqueue{}
	after := orchestration.NewSpawner(s, nil)
	after.SetEnqueue(enq.fn)
	resumed, err := governedUnits(after).Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("resumed spawn: %v", err)
	}
	if resumed != first {
		t.Fatalf("the resumed run spawned a different child: %q then %q", first, resumed)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("the resumed run created a second child: %v", enq.keys)
	}

	// The adopted child is still the spawner's to poll and to release.
	out, err := governedUnits(after).Outcomes(context.Background(), []string{resumed})
	if err != nil {
		t.Fatalf("outcomes: %v", err)
	}
	if len(out) != 1 || out[0].Done {
		t.Fatalf("an adopted child that has not finished reads as done: %+v", out)
	}
}

// A unit spawn is governed at the same waist a model's spawn is: nothing about the plan
// path is a way around the depth guard.
func TestUnitSpawnIsRefusedPastTheDepthGuard(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil, orchestration.WithMaxDepth(2))
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "deep", goal.Spec{Objective: "lead", StopCondition: "done", Depth: 2})

	_, err := governedUnits(sp).Spawn(context.Background(), parent, unit("parser"))
	if err == nil {
		t.Fatal("a unit was spawned past the delegation depth")
	}
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("classified %v, want Forbidden so the graph settles the unit rather than retrying", got)
	}
}

// The invariant the plan path exists under: writing a plan is not a way around the
// grant. A goal whose grant omits the right to fan out is refused here exactly as its
// model would be refused calling the spawn tool, and no child is created.
func TestUnitSpawnIsRefusedWithoutTheSpawnCapability(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)
	parent := putParent(t, s, "root", goal.Spec{
		Objective: "lead", StopCondition: "done",
		Grant: []string{"read", mission.ActionModelGenerate}, // everything but spawn
	})

	_, err := governedUnits(sp).Spawn(context.Background(), parent, unit("parser"))
	if err == nil {
		t.Fatal("a goal with no spawn capability fanned out by writing a plan")
	}
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("classified %v, want Forbidden", got)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("a refused spawn created a child anyway: %v", enq.keys)
	}
}

// A fan-out with no waist wired is refused rather than run ungoverned. The alternative
// is a spawn path that works and is not admitted against anything, which is the shape of
// every governance bypass that got shipped by accident.
func TestUngovernedUnitFanoutRefusesToSpawn(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})

	_, err := orchestration.Units(sp, nil).Spawn(context.Background(), parent, unit("parser"))
	if err == nil {
		t.Fatal("an ungoverned fan-out spawned a child")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("classified %v, want Terminal", got)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("an ungoverned fan-out created a child: %v", enq.keys)
	}
}

// unreadableGet is a store whose reads fail for a reason that is not "no such record",
// which is the case the idempotency lookup has to tell apart from a child that simply
// does not exist yet.
type unreadableGet struct{ resource.Store }

func (unreadableGet) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return resource.Resource{}, errors.New("the store is unreachable")
}

// A lookup that failed is not a child that is absent. Creating one on a read that never
// answered is how a fan-out doubles across a network blip, so the spawn retries instead.
func TestUnitSpawnRetriesAFailedIdempotencyLookup(t *testing.T) {
	s := newStore(t)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(unreadableGet{Store: s}, nil)
	sp.SetEnqueue(enq.fn)

	_, err := governedUnits(sp).Spawn(context.Background(), parent, unit("parser"))
	if err == nil {
		t.Fatal("a spawn went ahead on a lookup that never answered")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("classified %v, want Transient", got)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("a child was created behind a failed lookup: %v", enq.keys)
	}
}

// A parent whose spec cannot be read has no grant to bind and no plan to trust, so the
// spawn is refused rather than run against a decoded-to-zero parent, which would be a
// child with no authority narrowing behind it.
func TestUnitSpawnRefusesAnUnreadableParent(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)
	parent := resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "root",
		Spec: []byte(`["this is not a goal spec"]`),
	}

	_, err := governedUnits(sp).Spawn(context.Background(), parent, unit("parser"))
	if err == nil {
		t.Fatal("a unit was spawned from a parent whose spec could not be read")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("classified %v, want Terminal", got)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("a child was created for an unreadable parent: %v", enq.keys)
	}
}

// A child proves its unit by its ledger, and only by its ledger. Every other way a
// child can finish is a failure that names itself, including converging: the child's
// phase says its own loop stopped, and the unit's question is whether the check the
// plan wrote was recorded as passing.
func TestUnitOutcomeIsReadFromTheChildsLedger(t *testing.T) {
	tests := []struct {
		name     string
		phase    goal.Phase
		message  string
		ledger   []goal.LedgerState
		evidence string
		failure  string
	}{
		{
			name:     "proven",
			phase:    goal.PhaseConverged,
			ledger:   []goal.LedgerState{proven("item-1", "spine:0199")},
			evidence: "spine:0199",
		},
		{
			name:     "every item proven",
			phase:    goal.PhaseConverged,
			ledger:   []goal.LedgerState{proven("item-1", "spine:0199"), proven("item-2", "spine:0200")},
			evidence: "spine:0199,spine:0200",
		},
		{
			name:    "converged unproven",
			phase:   goal.PhaseConverged,
			ledger:  []goal.LedgerState{{ID: "item-1"}},
			failure: "1 of 1 ledger item(s) unproven",
		},
		{
			name:    "proven with nothing behind it",
			phase:   goal.PhaseConverged,
			ledger:  []goal.LedgerState{proven("item-1", "")},
			failure: "1 of 1 ledger item(s) unproven",
		},
		{
			name:    "converged with no ledger",
			phase:   goal.PhaseConverged,
			failure: "no ledger to prove its unit",
		},
		{
			name:    "stalled",
			phase:   goal.PhaseStalled,
			message: "the build never compiled",
			ledger:  []goal.LedgerState{proven("item-1", "spine:0199")},
			failure: "the build never compiled",
		},
		{
			name:    "stalled silently",
			phase:   goal.PhaseStalled,
			failure: "stalled without recording why",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			sp := orchestration.NewSpawner(s, nil)
			sp.SetEnqueue((&recordingEnqueue{}).fn)
			parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})
			units := governedUnits(sp)

			id, err := units.Spawn(context.Background(), parent, unit("parser"))
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			settleLedger(t, s, id, tc.phase, tc.message, tc.ledger)

			out, err := units.Outcomes(context.Background(), []string{id})
			if err != nil {
				t.Fatalf("outcomes: %v", err)
			}
			if len(out) != 1 || !out[0].Done {
				t.Fatalf("a finished child was not reported done: %+v", out)
			}
			if out[0].Evidence != tc.evidence {
				t.Fatalf("evidence = %q, want %q", out[0].Evidence, tc.evidence)
			}
			if tc.failure == "" && out[0].Failure != "" {
				t.Fatalf("a proven unit carried a failure: %q", out[0].Failure)
			}
			if tc.failure != "" && !strings.Contains(out[0].Failure, tc.failure) {
				t.Fatalf("failure %q does not say %q", out[0].Failure, tc.failure)
			}
		})
	}
}

// A child still running is reported not done, which is what keeps its parent parked
// rather than settling a unit on a partial run.
func TestUnitOutcomeLeavesARunningChildRunning(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})
	units := governedUnits(sp)

	id, err := units.Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	settleLedger(t, s, id, goal.PhaseRunning, "", nil)

	out, err := units.Outcomes(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("outcomes: %v", err)
	}
	if len(out) != 1 || out[0].Done || out[0].Evidence != "" {
		t.Fatalf("a running child was reported as an outcome: %+v", out)
	}
}

// Polling a child the store does not have is a read that failed, classified so the
// reconciler retries it rather than settling the unit over a question nobody answered.
func TestUnitOutcomesRetryAFailedRead(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)

	_, err := governedUnits(sp).Outcomes(context.Background(), []string{"no-such-child"})
	if err == nil {
		t.Fatal("a child that could not be read was reported as an outcome")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("classified %v, want Transient", got)
	}
}

// A child whose status cannot be decoded is not a graph that stopped, and it is not
// retryable either: it is a record this fan-out cannot act on at all.
func TestUnitOutcomesRefuseAnUnreadableChild(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})
	units := governedUnits(sp)

	id, err := units.Spawn(context.Background(), parent, unit("parser"))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	r, err := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = []byte(`["this is not a goal status"]`)
	if _, err := s.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	if _, err := units.Outcomes(context.Background(), []string{id}); err == nil {
		t.Fatal("a child whose status could not be read was reported as an outcome")
	} else if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("classified %v, want Terminal", got)
	}
}

// A unit missing its verify clause has no ledger to be judged against, so the spawn is
// refused before a child exists. The graph refuses this at admission too; this is the
// spawner not depending on that having happened.
func TestUnitSpawnRefusesAUnitWithNothingToProve(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "lead", StopCondition: "done"})

	_, err := governedUnits(sp).Spawn(context.Background(), parent, goal.Unit{ID: "parser", Objective: "do it"})
	if err == nil {
		t.Fatal("a unit with no verify clause was spawned")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("classified %v, want Terminal", got)
	}
	if len(enq.keys) != 0 {
		t.Fatalf("a child was created for a unit that could never be proven: %v", enq.keys)
	}
}
