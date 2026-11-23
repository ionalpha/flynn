package skilltool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
)

// A loaded skill puts the same text in front of the model as a read one, and it is not
// a read: Reads is what a run's outcome is credited to, and a body the model never
// asked for is no evidence the model chose it.
func TestLoadIsTheReadTextWithoutTheRead(t *testing.T) {
	skills := state.NewMemory().Skills()
	ctx := context.Background()
	if _, err := skills.Upsert(ctx, bundledSkill()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	set := skilltool.New(skills, skilltool.WithPack(pack(), "skills"))

	loaded, err := set.Load(ctx, "tidy-diff")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := set.Reads(); len(got) != 0 {
		t.Fatalf("a load was recorded as a read: %v", got)
	}

	var read mission.Tool
	for _, tool := range set.Tools() {
		if tool.Def().Name == "skill_read" {
			read = tool
		}
	}
	viaRead, err := call(t, read, map[string]string{"skill": "tidy-diff"})
	if err != nil {
		t.Fatalf("skill_read: %v", err)
	}
	if loaded != viaRead {
		t.Errorf("load and skill_read differ:\nload: %q\nread: %q", loaded, viaRead)
	}

	if _, err := set.Load(ctx, "no-such-skill"); !errors.Is(err, skilltool.ErrUnknownSkill) {
		t.Errorf("loading an unknown skill: err = %v, want ErrUnknownSkill", err)
	}
}
