package main

import (
	"testing"

	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// TestMissionPartsGrantsSpawnOnlyForFanout locks the governance invariant the shared
// builder exists to protect: a single-conversation run's grant must NOT include the
// delegation action, and a fan-out run's grant MUST. Both must always cover the model
// call and the distillation, plus every tool. Before the builder, the two assembly
// paths hand-assembled these lists, so a new action added to one and not the other
// would silently diverge what single vs fan-out runs are allowed to do; this test
// fails if that divergence ever returns.
func TestMissionPartsGrantsSpawnOnlyForFanout(t *testing.T) {
	skills := skilltool.New(state.NewMemory().Skills())
	single, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), skills, "", false, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("newMissionParts single: %v", err)
	}
	fanout, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), skills, "", true, sandbox.ResourceLimits{})
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

// TestMissionPartsSkillToolsFollowTheStore locks the other half of the same
// invariant: the skill tools are offered when there is a store to answer them and
// withheld when there is not. A run that offered skill_read with nothing behind it
// would advertise a capability every call fails at, which is worse than not having
// it, and the grant must track the offer either way.
func TestMissionPartsSkillToolsFollowTheStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		skills *skilltool.Set
		want   bool
	}{
		{"with a store", skilltool.New(state.NewMemory().Skills()), true},
		{"without one", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), tc.skills, "", false, sandbox.ResourceLimits{})
			if err != nil {
				t.Fatalf("newMissionParts: %v", err)
			}
			for _, want := range []string{"skill_read", "skill_resource"} {
				var offered bool
				for _, tool := range parts.toolset {
					if tool.Def().Name == want {
						offered = true
					}
				}
				if offered != tc.want {
					t.Errorf("%s offered = %v, want %v", want, offered, tc.want)
				}
				if got := parts.grant.Allows(want); got != tc.want {
					t.Errorf("grant allows %s = %v, want %v", want, got, tc.want)
				}
			}
		})
	}
}
