package skill

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

func TestTriggersCrossThePackBoundary(t *testing.T) {
	want := []state.SkillTrigger{{Kind: "file", Match: "go.mod"}, {Kind: "repeat", Count: 3}}
	d, err := ToDoc(state.Skill{Slug: "gate", Description: "d", Triggers: want})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Metadata[skillmd.MetaTriggers]; !ok {
		t.Fatal("ToDoc dropped the triggers")
	}
	got, err := FromDoc(d, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Triggers, want) {
		t.Fatalf("FromDoc = %+v, want %+v", got.Triggers, want)
	}
}

func TestFromDocRefusesMalformedTriggers(t *testing.T) {
	d := skillmd.Doc{Name: "gate", Description: "d", Metadata: map[string]string{skillmd.MetaTriggers: `[{"kind":"repeat","count":1}]`}}
	if _, err := FromDoc(d, state.Scope{}); !errors.Is(err, skillmd.ErrInvalid) {
		t.Fatalf("FromDoc = %v, want ErrInvalid", err)
	}
}

func TestFromDocKeepsKnownTriggersBesideUnknown(t *testing.T) {
	d := skillmd.Doc{Name: "gate", Description: "d", Metadata: map[string]string{skillmd.MetaTriggers: `[{"kind":"from-the-future"},{"kind":"tool","match":"shell"}]`}}
	got, err := FromDoc(d, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Triggers) != 1 || got.Triggers[0].Match != "shell" {
		t.Fatalf("got %+v, want the one known trigger", got.Triggers)
	}
}
