package grade_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/grade"
	"github.com/ionalpha/flynn/spine"
)

// pass and fail are fixed checks for exercising ladder accumulation.
func pass(name string) grade.Check {
	return func(context.Context, grade.Evidence) (bool, string, error) { return true, name, nil }
}

func fail(name string) grade.Check {
	return func(context.Context, grade.Evidence) (bool, string, error) { return false, name, nil }
}

func ctx() context.Context { return context.Background() }

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}

func TestCumulativeAllPassScoresOne(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Mode: grade.Cumulative, Milestones: []grade.Milestone{
		{Name: "find", Check: pass("find")},
		{Name: "reproduce", Check: pass("reproduce")},
		{Name: "exploit", Check: pass("exploit")},
	}}
	g, err := l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !approxEqual(g.Score, 1) || g.Reached != 3 {
		t.Fatalf("score=%v reached=%d, want 1.0 and 3", g.Score, g.Reached)
	}
	for _, r := range g.Rungs {
		if !r.Passed || !r.Reached {
			t.Fatalf("rung %q not passed/reached: %+v", r.Name, r)
		}
	}
}

func TestCumulativeStopsAtFirstFailure(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Mode: grade.Cumulative, Milestones: []grade.Milestone{
		{Name: "find", Check: pass("find")},
		{Name: "reproduce", Check: fail("reproduce")},
		{Name: "exploit", Check: pass("exploit")}, // would pass, but must not be reached
	}}
	g, err := l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !approxEqual(g.Score, 1.0/3.0) {
		t.Fatalf("score=%v, want 1/3", g.Score)
	}
	if g.Reached != 1 {
		t.Fatalf("reached=%d, want 1", g.Reached)
	}
	if g.Rungs[2].Reached {
		t.Fatal("rung after the first failure must not be reached")
	}
}

func TestIndependentCreditsNonContiguous(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Mode: grade.Independent, Milestones: []grade.Milestone{
		{Name: "a", Check: pass("a")},
		{Name: "b", Check: fail("b")},
		{Name: "c", Check: pass("c")}, // reached and credited despite b failing
	}}
	g, err := l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !approxEqual(g.Score, 2.0/3.0) || g.Reached != 2 {
		t.Fatalf("score=%v reached=%d, want 2/3 and 2", g.Score, g.Reached)
	}
	if !g.Rungs[2].Reached || !g.Rungs[2].Passed {
		t.Fatalf("rung c should be reached and passed: %+v", g.Rungs[2])
	}
}

func TestWeightsRespected(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Mode: grade.Cumulative, Milestones: []grade.Milestone{
		{Name: "cheap", Weight: 1, Check: pass("cheap")},
		{Name: "dear", Weight: 3, Check: pass("dear")},
	}}
	g, err := l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !approxEqual(g.Attained, 4) || !approxEqual(g.Total, 4) || !approxEqual(g.Score, 1) {
		t.Fatalf("attained=%v total=%v score=%v, want 4/4/1", g.Attained, g.Total, g.Score)
	}

	// Failing the heavy rung first yields no credit; total still counts every weight.
	l.Milestones = []grade.Milestone{
		{Name: "dear", Weight: 3, Check: fail("dear")},
		{Name: "cheap", Weight: 1, Check: pass("cheap")},
	}
	g, err = l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !approxEqual(g.Attained, 0) || !approxEqual(g.Total, 4) || !approxEqual(g.Score, 0) {
		t.Fatalf("attained=%v total=%v score=%v, want 0/4/0", g.Attained, g.Total, g.Score)
	}
}

func TestZeroWeightDefaultsToOne(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Milestones: []grade.Milestone{
		{Name: "a", Check: pass("a")}, // Weight 0 -> 1
		{Name: "b", Check: pass("b")},
	}}
	g, _ := l.Grade(ctx(), grade.MapEvidence{})
	if !approxEqual(g.Total, 2) {
		t.Fatalf("total=%v, want 2 (zero weight defaults to 1)", g.Total)
	}
}

func TestEmptyLadderScoresZeroOverZero(t *testing.T) {
	g, err := (grade.Ladder{Name: "empty"}).Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if g.Score != 0 || g.Total != 0 || len(g.Rungs) != 0 {
		t.Fatalf("empty ladder = %+v, want zeroed", g)
	}
}

func TestCheckErrorAbortsGrading(t *testing.T) {
	boom := errors.New("boom")
	l := grade.Ladder{Name: "ladder", Milestones: []grade.Milestone{
		{Name: "ok", Check: pass("ok")},
		{Name: "bad", Check: func(context.Context, grade.Evidence) (bool, string, error) { return false, "", boom }},
	}}
	if _, err := l.Grade(ctx(), grade.MapEvidence{}); !errors.Is(err, boom) {
		t.Fatalf("grade error = %v, want wrapped boom", err)
	}
}

func TestNilCheckFails(t *testing.T) {
	l := grade.Ladder{Name: "ladder", Milestones: []grade.Milestone{{Name: "a"}}}
	g, err := l.Grade(ctx(), grade.MapEvidence{})
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if g.Rungs[0].Passed {
		t.Fatal("a nil check must not pass")
	}
}

func TestEventRecordedCheck(t *testing.T) {
	ev := grade.MapEvidence{Recorded: []spine.Event{
		{Type: "file.written", Payload: map[string]any{"path": "exploit.py"}},
		{Type: "command.exited", Payload: map[string]any{"code": float64(0)}},
	}}
	// Any event of the type.
	if ok, _, _ := grade.EventRecorded("file.written", nil)(ctx(), ev); !ok {
		t.Fatal("expected file.written to be recorded")
	}
	// Type present but payload predicate rejects.
	clean := grade.EventRecorded("command.exited", func(p map[string]any) bool { return p["code"] == float64(0) })
	if ok, _, _ := clean(ctx(), ev); !ok {
		t.Fatal("expected a clean exit to match")
	}
	// Type absent.
	if ok, _, _ := grade.EventRecorded("network.connect", nil)(ctx(), ev); ok {
		t.Fatal("absent event type must not match")
	}
}

func TestAnswerChecks(t *testing.T) {
	ev := grade.MapEvidence{Answers: map[string]string{
		"flag":     "  FLAG{Sql_Injection} ",
		"location": "auth.go:42",
	}}
	if ok, _, _ := grade.AnswerEquals("flag", "flag{sql_injection}")(ctx(), ev); !ok {
		t.Fatal("AnswerEquals should normalize whitespace and case")
	}
	if ok, _, _ := grade.AnswerEquals("flag", "wrong")(ctx(), ev); ok {
		t.Fatal("a wrong answer must not match")
	}
	if ok, _, _ := grade.AnswerEquals("missing", "x")(ctx(), ev); ok {
		t.Fatal("a missing answer must not match")
	}
	hasLine := grade.AnswerMatches("location", func(a string) bool { return a == "auth.go:42" })
	if ok, _, _ := hasLine(ctx(), ev); !ok {
		t.Fatal("AnswerMatches should accept the matching answer")
	}
}
