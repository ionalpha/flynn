package orchestration_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
)

func TestSpawnInheritsTheParentsConfinement(t *testing.T) {
	s := newStore(t)
	sp := orchestration.NewSpawner(s, nil)
	sp.SetEnqueue((&recordingEnqueue{}).fn)
	want := &goal.Confinement{Workdir: t.TempDir(), Grant: []string{"read", "spawn"}}
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c", Confinement: want})

	id, err := sp.Spawn(context.Background(), parent, mission.SubGoal{Objective: "child"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	got := childSpec(t, s, id).Confinement
	if got == nil || got.Covers(want) != nil || want.Covers(got) != nil {
		t.Fatalf("child confinement = %+v, want the parent's %+v", got, want)
	}
}
