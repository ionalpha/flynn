package orchestration_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
)

func storeWithAgents(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	for _, reg2 := range []func(*resource.Registry) error{
		resource.RegisterCoreKinds, goal.RegisterKind, budget.RegisterKind, archetype.RegisterKind,
	} {
		if err := reg2(reg); err != nil {
			t.Fatal(err)
		}
	}
	return resource.NewMemory(reg)
}

func putAgentRes(t *testing.T, s resource.Store, name string, spec archetype.Spec) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), resource.Resource{
		APIVersion: archetype.GroupVersion, Kind: archetype.Kind, Name: name, Spec: raw,
	}); err != nil {
		t.Fatalf("put agent: %v", err)
	}
}

// TestSpawnNamedAgentConfiguresChild proves a delegation to a named Agent runs the
// child as that archetype: its system prompt is baked onto the child, and the
// child's grant is the Agent's capabilities intersected with the parent's authority
// (never widened).
func TestSpawnNamedAgentConfiguresChild(t *testing.T) {
	s := storeWithAgents(t)
	putAgentRes(t, s, "researcher", archetype.Spec{
		System:       "You research and report.",
		Capabilities: []string{"read", "glob"},
	})
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	// Parent can read/glob/write/spawn; the researcher Agent only read/glob.
	parent := putParent(t, s, "root", goal.Spec{
		Objective: "lead", StopCondition: "done",
		Grant: []string{"read", "glob", "write", "spawn"},
	})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{
		Objective: "find the cause", Agent: "researcher",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	cs := childSpec(t, s, id)
	if cs.System != "You research and report." {
		t.Fatalf("child system = %q, want the agent's prompt", cs.System)
	}
	got := cs.Grant
	sort.Strings(got)
	if len(got) != 2 || got[0] != "glob" || got[1] != "read" {
		t.Fatalf("child grant = %v, want [glob read] (agent caps ∩ parent)", got)
	}
}

// TestSpawnNamedAgentCannotWidenPastParent proves the Agent's capabilities are
// still bounded by the parent: an Agent that wants more than the parent holds gets
// only the intersection.
func TestSpawnNamedAgentCannotWidenPastParent(t *testing.T) {
	s := storeWithAgents(t)
	putAgentRes(t, s, "power", archetype.Spec{Capabilities: []string{"read", "write", "bash"}})
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read", "write"}})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Agent: "power"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	got := childSpec(t, s, id).Grant
	sort.Strings(got)
	if len(got) != 2 || got[0] != "read" || got[1] != "write" {
		t.Fatalf("child grant = %v, want [read write] (bash dropped, not in parent)", got)
	}
}

// TestSpawnUnknownAgentFailsClosed proves a delegation to a nonexistent Agent is
// refused, not silently run as an ad-hoc child.
func TestSpawnUnknownAgentFailsClosed(t *testing.T) {
	s := storeWithAgents(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read"}})

	_, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Agent: "ghost"})
	if fault.Classify(err) != fault.Forbidden {
		t.Fatalf("spawning an unknown agent should fail closed (Forbidden), got: %v", err)
	}
}

// TestSpawnNamedAgentComposes proves the resolution flattens composition: a
// specialist extending a base gets the union of capabilities (intersected with the
// parent).
func TestSpawnNamedAgentComposes(t *testing.T) {
	s := storeWithAgents(t)
	putAgentRes(t, s, "base", archetype.Spec{System: "base", Capabilities: []string{"read"}})
	putAgentRes(t, s, "specialist", archetype.Spec{
		Extends: []string{"base"}, System: "specialist", Capabilities: []string{"glob"},
	})
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Grant: []string{"read", "glob", "write"}})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "x", Agent: "specialist"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	cs := childSpec(t, s, id)
	if cs.System != "specialist" {
		t.Fatalf("child system = %q, want the specialist override", cs.System)
	}
	got := cs.Grant
	sort.Strings(got)
	if len(got) != 2 || got[0] != "glob" || got[1] != "read" {
		t.Fatalf("child grant = %v, want [glob read] (composed base+specialist ∩ parent)", got)
	}
}
