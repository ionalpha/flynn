package main

import (
	"testing"

	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// TestMissionPartsGrantsSpawnOnlyForFanout locks the governance invariant the shared
// builder exists to protect: a single-conversation run's grant must NOT include the
// delegation action, and a fan-out run's grant MUST. Both must always cover the model
// call and the distillation, plus every tool. Before the builder, the two assembly
// paths hand-assembled these lists, so a new action added to one and not the other
// would silently diverge what single vs fan-out runs are allowed to do; this test
// fails if that divergence ever returns.
func TestMissionPartsGrantsSpawnOnlyForFanout(t *testing.T) {
	single, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), "", false, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("newMissionParts single: %v", err)
	}
	fanout, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), "", true, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("newMissionParts fanout: %v", err)
	}

	// The delegation action is the one difference between the two paths.
	if single.grant.Allows(mission.ActionSpawn) {
		t.Error("single-conversation grant must not allow the spawn action")
	}
	if !fanout.grant.Allows(mission.ActionSpawn) {
		t.Error("fan-out grant must allow the spawn action")
	}

	// Everything else is identical: both cover the model call, the distillation, and
	// every tool in the shared toolset.
	for _, p := range []struct {
		name  string
		parts *missionParts
	}{{"single", single}, {"fanout", fanout}} {
		if !p.parts.grant.Allows(mission.ActionModelGenerate) {
			t.Errorf("%s grant must allow the model-generate action", p.name)
		}
		if !p.parts.grant.Allows(learn.DistillAction) {
			t.Errorf("%s grant must allow the distill action", p.name)
		}
		for _, tool := range p.parts.toolset {
			if name := tool.Def().Name; !p.parts.grant.Allows(name) {
				t.Errorf("%s grant must allow tool %q", p.name, name)
			}
		}
	}

	// The two toolsets are assembled the same way, so a fan-out run's grant is exactly
	// the single run's plus the spawn action - never more, never less.
	if got, want := len(fanout.grant.Actions()), len(single.grant.Actions())+1; got != want {
		t.Errorf("fan-out grant has %d actions, want single (%d) + 1", got, len(single.grant.Actions()))
	}
}
