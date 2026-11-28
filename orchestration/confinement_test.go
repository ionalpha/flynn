package orchestration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
)

// A spawner that did not spawn an unfinished child, as after a restart, enqueues
// it once, by either way of asking after it, and reports a failure to.
func TestAnUnfinishedChildFromAnEarlierProcessIsTakenUp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	first := orchestration.NewSpawner(s, nil)
	first.SetEnqueue((&recordingEnqueue{}).fn)
	parent := putParent(t, s, "root", goal.Spec{Objective: "o", StopCondition: "c"})
	id, err := first.Spawn(ctx, parent, mission.SubGoal{Objective: "child"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	broken := errors.New("no queue")
	ask := map[string]func(*orchestration.Spawner) error{
		"poll": func(sp *orchestration.Spawner) error {
			_, _, err := sp.Poll(ctx, []string{id})
			return err
		},
		"outcomes": func(sp *orchestration.Spawner) error {
			_, err := orchestration.Units(sp, nil).Outcomes(ctx, []string{id})
			return err
		},
	}
	for name, check := range ask {
		rec := &recordingEnqueue{}
		later := orchestration.NewSpawner(s, nil)
		later.SetEnqueue(func(context.Context, resource.Key) error { return broken })
		if err := check(later); !errors.Is(err, broken) {
			t.Errorf("%s = %v, want the enqueue failure", name, err)
		}
		later.SetEnqueue(rec.fn)
		for range 2 {
			if err := check(later); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if len(rec.keys) != 1 || rec.keys[0].Name != id {
			t.Errorf("%s enqueued %v, want %s once", name, rec.keys, id)
		}
	}
}

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
