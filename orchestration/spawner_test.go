package orchestration_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
)

func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

// recordingEnqueue captures the keys handed to the runtime.
type recordingEnqueue struct{ keys []resource.Key }

func (r *recordingEnqueue) fn(_ context.Context, key resource.Key) error {
	r.keys = append(r.keys, key)
	return nil
}

// putParent stores a parent goal with the given spec and returns it.
func putParent(t *testing.T, s resource.Store, name string, spec goal.Spec) resource.Resource {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Put(context.Background(), resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name, Spec: raw,
	})
	if err != nil {
		t.Fatalf("put parent: %v", err)
	}
	return r
}

func childSpec(t *testing.T, s resource.Store, id string) goal.Spec {
	t.Helper()
	r, err := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatalf("decode child: %v", err)
	}
	return spec
}

// settle sets a child's terminal phase + message, as the reconciler would.
func settle(t *testing.T, s resource.Store, id string, phase goal.Phase, msg string) {
	t.Helper()
	r, err := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := goal.Status{Phase: phase, Message: msg}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r.Status = enc
	if _, err := s.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnCreatesGovernedChild(t *testing.T) {
	s := newStore(t)
	enq := &recordingEnqueue{}
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue(enq.fn)

	parent := putParent(t, s, "root", goal.Spec{
		Objective: "lead", StopCondition: "done",
		Grant: []string{"read", "write", "bash", "spawn"}, Depth: 0,
	})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{
		Objective: "research the thing", Actions: []string{"read", "write"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Enqueued for reconciliation.
	if len(enq.keys) != 1 || enq.keys[0].Name != id {
		t.Fatalf("child not enqueued: %v", enq.keys)
	}

	cs := childSpec(t, s, id)
	// Grant narrowed to the requested subset of the parent's authority (no bash/spawn).
	sort.Strings(cs.Grant)
	if got := cs.Grant; len(got) != 2 || got[0] != "read" || got[1] != "write" {
		t.Fatalf("child grant = %v, want [read write]", got)
	}
	if cs.Depth != 1 {
		t.Fatalf("child depth = %d, want 1", cs.Depth)
	}
	if cs.BudgetPool != "root" {
		t.Fatalf("child pool = %q, want root", cs.BudgetPool)
	}

	// Owned by the parent for cascade teardown.
	cr, _ := s.Get(context.Background(), goal.Kind, resource.Scope{}, id)
	if len(cr.OwnerReferences) != 1 || cr.OwnerReferences[0].Name != "root" || !cr.OwnerReferences[0].Controller {
		t.Fatalf("child owner refs = %+v, want a controller ref to root", cr.OwnerReferences)
	}
}

func TestSpawnCannotWidenAuthority(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read"}})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{
		Objective: "x", Actions: []string{"read", "write", "bash"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := childSpec(t, s, id).Grant; len(got) != 1 || got[0] != "read" {
		t.Fatalf("child grant = %v, want [read] (cannot widen past parent)", got)
	}
}

func TestSpawnDepthGuard(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil, orchestration.WithMaxDepth(2))
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Depth: 2})

	_, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x"})
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("spawn at depth 3 with max 2 should be Forbidden, got %s: %v", got, err)
	}
}

func TestSpawnIsBudgetBounded(t *testing.T) {
	s := newStore(t)
	l := budget.NewLedger(s)
	if _, err := l.Open(context.Background(), "root", resource.Scope{}, budget.Limits{Tokens: 100}); err != nil {
		t.Fatal(err)
	}
	sp := orchestration.NewSpawner(s, l, orchestration.WithReservation(budget.Spent{Tokens: 60}))
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read"}})

	// Each child reserves 60 against the 100-token pool: two admit (committed 60 then
	// 120, each under 100 when checked), the third is refused.
	mustSpawn(t, sp, parent)
	mustSpawn(t, sp, parent)
	_, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Actions: []string{"read"}})
	if got := fault.Classify(err); got != fault.BudgetExceeded {
		t.Fatalf("third spawn should be BudgetExceeded, got %s: %v", got, err)
	}
}

func TestSpawnConcurrencyCapReleasesOnFinish(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil, orchestration.WithConcurrency(1))
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read"}})

	id := mustSpawn(t, sp, parent)
	// At capacity: the second spawn is refused.
	if _, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Actions: []string{"read"}}); fault.Classify(err) != fault.Forbidden {
		t.Fatalf("second spawn at capacity should be Forbidden, got %v", err)
	}
	// The first child finishes; Poll releases its slot.
	settle(t, s, id, goal.PhaseConverged, "done")
	if _, _, err := sp.Poll(context.Background(), []string{id}); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// A slot is free again.
	if _, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "y", Actions: []string{"read"}}); err != nil {
		t.Fatalf("spawn after release should succeed: %v", err)
	}
}

func TestPollReportsOutcomes(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read"}})

	ok := mustSpawn(t, sp, parent)
	bad := mustSpawn(t, sp, parent)
	running := mustSpawn(t, sp, parent)
	settle(t, s, ok, goal.PhaseConverged, "the answer")
	settle(t, s, bad, goal.PhaseStalled, "step failed")
	settle(t, s, running, goal.PhaseRunning, "")

	results, allDone, err := sp.Poll(context.Background(), []string{ok, bad, running})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if allDone {
		t.Fatal("a running child means not all done")
	}
	byID := map[string]mission.ChildResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if r := byID[ok]; r.Failed || r.Result != "the answer" {
		t.Fatalf("converged child = %+v", r)
	}
	if r := byID[bad]; !r.Failed {
		t.Fatalf("stalled child should be failed: %+v", r)
	}
}

// TestNarrowGrantProperty is the rigor property: a child's grant is always a subset
// of both what its parent holds and what the sub-goal requested, so a delegation can
// never widen authority.
func TestNarrowGrantProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		actions := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{1,4}`), 0, 8, func(s string) string { return s }).Draw(rt, "actions")
		parentGrant := pickSubset(rt, actions, "parent")
		requested := pickSubset(rt, actions, "requested")

		s := newStore(t)
		sp := orchestration.NewSpawner(s, nil)
		sp.SetEnqueue((&recordingEnqueue{}).fn)
		parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: parentGrant})

		id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Actions: requested})
		if err != nil {
			rt.Fatalf("spawn: %v", err)
		}
		got := childSpec(t, s, id).Grant
		pset := toSet(parentGrant)
		rset := toSet(requested)
		for _, a := range got {
			// When the parent is unconstrained (empty grant), the child is scoped to its
			// request; otherwise it is the intersection.
			if len(parentGrant) > 0 && !pset[a] {
				rt.Fatalf("child action %q not in parent grant %v", a, parentGrant)
			}
			if !rset[a] {
				rt.Fatalf("child action %q not requested %v", a, requested)
			}
		}
	})
}

func mustSpawn(t *testing.T, sp *orchestration.Spawner, parent resource.Resource) string {
	t.Helper()
	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Actions: []string{"read"}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return id
}

func pickSubset(rt *rapid.T, from []string, label string) []string {
	var out []string
	for _, a := range from {
		if rapid.Bool().Draw(rt, label+"-"+a) {
			out = append(out, a)
		}
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}
