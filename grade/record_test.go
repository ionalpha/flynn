package grade_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/grade"
	"github.com/ionalpha/flynn/spine"
)

func TestRecordWritesRungThenSummaryEvents(t *testing.T) {
	ctx := context.Background()
	l := grade.Ladder{Name: "exploit", Mode: grade.Cumulative, Milestones: []grade.Milestone{
		{Name: "find", Check: pass("find")},
		{Name: "reproduce", Check: pass("reproduce")},
		{Name: "exploit", Check: fail("exploit")},
	}}
	g, err := l.Grade(ctx, grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	log := spine.NewMemoryLog()
	const stream = "run-1"
	if err := grade.Record(ctx, log, stream, g); err != nil {
		t.Fatalf("record: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// One event per rung, then a summary: 3 + 1 = 4.
	if len(events) != len(g.Rungs)+1 {
		t.Fatalf("events = %d, want %d", len(events), len(g.Rungs)+1)
	}
	for i := range g.Rungs {
		if events[i].Type != grade.EvMilestone {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, grade.EvMilestone)
		}
		if events[i].Actor != spine.ActorSystem {
			t.Fatalf("event %d actor = %q, want system", i, events[i].Actor)
		}
	}
	last := events[len(events)-1]
	if last.Type != grade.EvSummary {
		t.Fatalf("last event type = %q, want %q", last.Type, grade.EvSummary)
	}
	if score, ok := last.Payload["score"].(float64); !ok || !approxEqual(score, g.Score) {
		t.Fatalf("summary score = %v (ok=%v), want %v", last.Payload["score"], ok, g.Score)
	}
}
