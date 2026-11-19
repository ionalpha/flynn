package session

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

func skillEvents(t *testing.T, log spine.Log, id string) []Event {
	t.Helper()
	events, err := History(context.Background(), log, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, ev := range events {
		if ev.Kind == KindSkill {
			out = append(out, ev)
		}
	}
	return out
}

// TestSkillEventRecordedOnceAcrossRetries: a failed step re-runs its brief and reports
// the same load again; the stream keeps one record per goal, skill and route, and a
// different goal, skill or route is still recorded.
func TestSkillEventRecordedOnceAcrossRetries(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	s := New(log, bus.NewMemory(), WithID("run-1"))
	rep := s.Reporter()

	load := mission.Event{Kind: mission.EventSkill, Skill: "feedback-loop", Via: "trigger", Result: "same command 3 times"}
	rep.Report(ctx, load)
	rep.Report(ctx, load)
	rep.Report(ctx, load)
	other := load
	other.Skill = "dependencies"
	rep.Report(ctx, other)
	offered := load
	offered.Via = "offered"
	rep.Report(ctx, offered)
	child := load
	child.Goal = "child-a"
	rep.Report(ctx, child)
	rep.Report(ctx, child)

	got := skillEvents(t, log, "run-1")
	if len(got) != 4 {
		t.Fatalf("skill events = %d, want 4 (trigger, other skill, offered, child): %+v", len(got), got)
	}
	first := got[0]
	if first.Skill != "feedback-loop" || first.Via != "trigger" || first.Reason != "same command 3 times" {
		t.Fatalf("first skill event = %+v, want the trigger load with its reason", first)
	}
}

func TestSkillEventMapsEveryField(t *testing.T) {
	se := toSessionEvent(mission.Event{Kind: mission.EventSkill, Goal: "g", Turn: 2, Skill: "s", Via: "skipped", Result: "needs shell"})
	if se.Kind != KindSkill || se.Goal != "g" || se.Turn != 2 || se.Skill != "s" || se.Via != "skipped" || se.Reason != "needs shell" {
		t.Fatalf("mapped = %+v", se)
	}
}
