package rubric_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/internal/rubric"
	"github.com/ionalpha/flynn/spine"
)

// failLog is a spine.Log whose Append fails once it has accepted failAfter events, for
// exercising the record error paths. Reads and snapshots are unused here.
type failLog struct {
	appends   int
	failAfter int
}

var errAppend = errors.New("append failed")

func (l *failLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	if l.appends >= l.failAfter {
		return spine.Event{}, errAppend
	}
	l.appends++
	return spine.Event{}, nil
}
func (l *failLog) Read(context.Context, spine.Query) ([]spine.Event, error) { return nil, nil }
func (l *failLog) SaveSnapshot(context.Context, spine.Snapshot) error       { return nil }
func (l *failLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

func TestRecordWritesIssuesAxesThenSummary(t *testing.T) {
	ctx := context.Background()
	r := rubric.Rubric{Name: "built", Max: 5, Threshold: 0.5, Axes: []rubric.Axis{
		{Name: "design", Weight: 1},
		{Name: "craft", Weight: 1},
	}}
	a := r.Assemble(map[string]rubric.RawScore{
		"design": {Score: 4, Reason: "clean"},
		"craft":  {Score: 2, Reason: "rough"},
	}, []rubric.Issue{{Axis: "craft", Severity: "minor", Detail: "no empty state"}})

	log := spine.NewMemoryLog()
	const stream = "run-1"
	if err := rubric.Record(ctx, log, stream, a); err != nil {
		t.Fatalf("record: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 1 issue + 2 axes + 1 summary = 4.
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	if events[0].Type != rubric.EvIssue {
		t.Fatalf("event 0 type = %q, want %q", events[0].Type, rubric.EvIssue)
	}
	for i := 1; i <= 2; i++ {
		if events[i].Type != rubric.EvAxis {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, rubric.EvAxis)
		}
		if events[i].Actor != spine.ActorSystem {
			t.Fatalf("event %d actor = %q, want system", i, events[i].Actor)
		}
	}
	last := events[3]
	if last.Type != rubric.EvSummary {
		t.Fatalf("last type = %q, want %q", last.Type, rubric.EvSummary)
	}
	if score, ok := last.Payload["overall"].(float64); !ok || !approx(score, a.Overall) {
		t.Fatalf("summary overall = %v (ok=%v), want %v", last.Payload["overall"], ok, a.Overall)
	}
	if passed, ok := last.Payload["passed"].(bool); !ok || passed != a.Passed {
		t.Fatalf("summary passed = %v, want %v", last.Payload["passed"], a.Passed)
	}
	// Every event is stamped with the grader fingerprint, so the record names what graded it.
	if fp, ok := last.Payload["fingerprint"].(string); !ok || fp != a.Fingerprint {
		t.Fatalf("summary fingerprint = %v, want %q", last.Payload["fingerprint"], a.Fingerprint)
	}
}

func TestRecordAppendErrorPropagates(t *testing.T) {
	ctx := context.Background()
	r := rubric.Rubric{Name: "r", Max: 5, Axes: []rubric.Axis{{Name: "a", Weight: 1}}}
	a := r.Assemble(map[string]rubric.RawScore{"a": {Score: 3}},
		[]rubric.Issue{{Axis: "a", Detail: "x"}})
	// Fail on the first append (the issue), then the second (the axis), then the third
	// (the summary): every return path in Record surfaces the error.
	for _, failAfter := range []int{0, 1, 2} {
		if err := rubric.Record(ctx, &failLog{failAfter: failAfter}, "s", a); !errors.Is(err, errAppend) {
			t.Fatalf("failAfter=%d: err = %v, want errAppend", failAfter, err)
		}
	}
}

func TestRecordNoIssuesStillWritesAxesAndSummary(t *testing.T) {
	ctx := context.Background()
	r := rubric.Rubric{Name: "r", Max: 5, Axes: []rubric.Axis{{Name: "a", Weight: 1}}}
	a := r.Assemble(map[string]rubric.RawScore{"a": {Score: 5}}, nil)

	log := spine.NewMemoryLog()
	if err := rubric.Record(ctx, log, "s", a); err != nil {
		t.Fatalf("record: %v", err)
	}
	events, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 0 issues + 1 axis + 1 summary = 2.
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
}
