package rubric_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/internal/rubric"
	"github.com/ionalpha/flynn/spine"
)

func TestRegradeHeldVerdictRecordsFreshVerdict(t *testing.T) {
	ctx := context.Background()
	prior := rubric.Assessment{Rubric: "r", Fingerprint: "old", Overall: 0.4, Passed: false}
	// The current grader now scores the same subject a pass.
	current := rubric.Assessment{Rubric: "r", Fingerprint: "new", Overall: 0.8, Passed: true}
	g := scriptedGrader{a: current}

	log := spine.NewMemoryLog()
	res, err := rubric.Regrade(ctx, log, "run-1", g, prior, rubric.Subject{Objective: "o", Result: "r"})
	if err != nil {
		t.Fatalf("regrade: %v", err)
	}
	if res.Held {
		t.Fatalf("verdict flipped fail->pass, Held should be false")
	}
	if !res.Regraded {
		t.Fatalf("fingerprint changed, Regraded should be true")
	}
	if !approx(res.ScoreDelta, 0.4) {
		t.Fatalf("delta = %v, want 0.4", res.ScoreDelta)
	}
	// The fresh verdict is recorded on the spine, stamped with the current fingerprint.
	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("regrade recorded nothing on the spine")
	}
	last := events[len(events)-1]
	if fp, _ := last.Payload["fingerprint"].(string); fp != "new" {
		t.Fatalf("recorded fingerprint = %v, want the current grader's", last.Payload["fingerprint"])
	}
}

func TestRegradeHeldWhenVerdictUnchanged(t *testing.T) {
	ctx := context.Background()
	prior := rubric.Assessment{Fingerprint: "fp", Overall: 0.8, Passed: true}
	current := rubric.Assessment{Fingerprint: "fp", Overall: 0.82, Passed: true}
	res, err := rubric.Regrade(ctx, nil, "", scriptedGrader{a: current}, prior, rubric.Subject{})
	if err != nil {
		t.Fatalf("regrade: %v", err)
	}
	if !res.Held {
		t.Fatalf("pass->pass should be Held")
	}
	if res.Regraded {
		t.Fatalf("same fingerprint should not count as Regraded")
	}
}

func TestRegradeNilGraderIsNoop(t *testing.T) {
	res, err := rubric.Regrade(context.Background(), nil, "", nil, rubric.Assessment{}, rubric.Subject{})
	if err != nil {
		t.Fatalf("nil grader should be a no-op, got %v", err)
	}
	if res.Held || res.Regraded || res.ScoreDelta != 0 {
		t.Fatalf("nil grader should return the zero result, got %+v", res)
	}
}

func TestRegradeGraderErrorPropagates(t *testing.T) {
	boom := errors.New("grader down")
	_, err := rubric.Regrade(context.Background(), nil, "", scriptedGrader{err: boom}, rubric.Assessment{}, rubric.Subject{})
	if !errors.Is(err, boom) {
		t.Fatalf("want the grader error, got %v", err)
	}
}

func TestRegradeRecordErrorPropagates(t *testing.T) {
	current := rubric.Assessment{Overall: 0.9, Passed: true}
	// The re-assessment succeeds but persisting it fails: the error must surface rather
	// than a silent re-grade that recorded nothing.
	_, err := rubric.Regrade(context.Background(), &failLog{failAfter: 0}, "s",
		scriptedGrader{a: current}, rubric.Assessment{}, rubric.Subject{})
	if !errors.Is(err, errAppend) {
		t.Fatalf("want the record error, got %v", err)
	}
}

func TestRegradeNilLogSkipsRecording(t *testing.T) {
	// A nil log means re-check without persisting: it must still reconcile the verdict.
	prior := rubric.Assessment{Overall: 0.2, Passed: false}
	current := rubric.Assessment{Overall: 0.9, Passed: true}
	res, err := rubric.Regrade(context.Background(), nil, "s", scriptedGrader{a: current}, prior, rubric.Subject{})
	if err != nil {
		t.Fatalf("regrade: %v", err)
	}
	if res.Held {
		t.Fatalf("fail->pass should not be Held")
	}
}
