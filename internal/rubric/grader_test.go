package rubric_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/rubric"
	"github.com/ionalpha/flynn/llm/llmtest"
)

func gradeCtx() context.Context { return context.Background() }

func TestModelGraderParsesVerdict(t *testing.T) {
	reply := `Here is my grade:
{"issues":[{"axis":"craft","severity":"minor","detail":"no empty state"}],
 "scores":[{"axis":"design","score":4,"reason":"clean"},{"axis":"craft","score":2,"reason":"rough"}]}`
	m := llmtest.NewScripted(llmtest.SayText(reply))
	r := rubric.Rubric{Name: "r", Max: 5, Threshold: 0.5, Axes: []rubric.Axis{
		{Name: "design", Weight: 1},
		{Name: "craft", Weight: 1},
	}}
	g := rubric.NewModelGrader(m, r)

	a, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "build a page", Result: "done"})
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if len(a.Issues) != 1 || a.Issues[0].Detail != "no empty state" {
		t.Fatalf("issues = %+v", a.Issues)
	}
	// (1*4/5 + 1*2/5)/2 = (0.8+0.4)/2 = 0.6.
	if !approx(a.Overall, 0.6) {
		t.Fatalf("overall = %v, want 0.6", a.Overall)
	}
	if !a.Passed {
		t.Fatalf("passed = false, want true")
	}
	if a.Axes[0].Reason != "clean" || a.Axes[1].Reason != "rough" {
		t.Fatalf("reasons lost: %+v", a.Axes)
	}
}

func TestModelGraderPromptCarriesConfessionAndCalibration(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"scores":[{"axis":"design","score":3,"reason":"ok"}]}`))
	r := rubric.Rubric{Name: "r", Max: 5, Axes: []rubric.Axis{{Name: "design", Weight: 2, Guide: "layout and hierarchy"}}, Bands: []rubric.Band{
		{Axis: "design", Score: 5, Reason: "a deliberate system"},
	}}
	g := rubric.NewModelGrader(m, r)
	if _, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "res", Evidence: "a diff"}); err != nil {
		t.Fatalf("assess: %v", err)
	}
	reqs := m.Requests()
	if len(reqs) != 1 {
		t.Fatalf("calls = %d, want 1", len(reqs))
	}
	// The confession pattern lives in the system prompt: name issues before scoring.
	if !strings.Contains(reqs[0].System, "CONFESS") {
		t.Fatalf("system prompt missing the confession instruction:\n%s", reqs[0].System)
	}
	user := reqs[0].Messages[0].TextContent()
	for _, want := range []string{"layout and hierarchy", "Calibration", "a deliberate system", "weight 2", "a diff"} {
		if !strings.Contains(user, want) {
			t.Fatalf("prompt missing %q:\n%s", want, user)
		}
	}
}

func TestGraderOptionsOverrideSystemAndMaxTokens(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"scores":[{"axis":"design","score":3,"reason":"ok"}]}`))
	r := rubric.Rubric{Name: "r", Max: 5, Axes: []rubric.Axis{{Name: "design", Weight: 1}}}
	g := rubric.NewModelGrader(m, r,
		rubric.WithGraderSystem("MY OWN GRADER FRAME"),
		rubric.WithGraderMaxTokens(2048),
		rubric.WithGraderSystem("   "), // blank is ignored, keeps the previous override
		rubric.WithGraderMaxTokens(0),  // non-positive is ignored
	)
	if _, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "r"}); err != nil {
		t.Fatalf("assess: %v", err)
	}
	req := m.Requests()[0]
	if req.System != "MY OWN GRADER FRAME" {
		t.Fatalf("system = %q, want the override (blank ignored)", req.System)
	}
	if req.MaxTokens != 2048 {
		t.Fatalf("maxTokens = %d, want 2048 (zero ignored)", req.MaxTokens)
	}
}

func TestModelGraderNoJSONIsTerminal(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText("looks good to me, ship it"))
	g := rubric.NewModelGrader(m, rubric.Default())
	_, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "r"})
	if err == nil {
		t.Fatalf("want error for reply with no JSON object")
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("class = %v, want terminal (a grader that returns nothing parseable is broken)", fault.Classify(err))
	}
}

func TestModelGraderMalformedJSONIsTerminal(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"scores":[{"axis":"design","score":}]}`))
	g := rubric.NewModelGrader(m, rubric.Default())
	_, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "r"})
	if err == nil || fault.Classify(err) != fault.Terminal {
		t.Fatalf("want terminal error for malformed JSON, got %v", err)
	}
}

func TestModelGraderModelErrorPropagates(t *testing.T) {
	// An empty script: the first Generate runs off the end and returns a terminal error.
	g := rubric.NewModelGrader(llmtest.NewScripted(), rubric.Default())
	if _, err := g.Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "r"}); err == nil {
		t.Fatalf("want the model error to propagate")
	}
}

func TestModelGraderIgnoresUnknownAxesAndScoresOmitted(t *testing.T) {
	// The model scores an axis the rubric does not have (ignored) and omits one it does
	// (left unscored, zero credit).
	m := llmtest.NewScripted(llmtest.SayText(
		`{"scores":[{"axis":"design","score":5,"reason":"great"},{"axis":"vibes","score":5,"reason":"n/a"}]}`))
	r := rubric.Rubric{Name: "r", Max: 5, Threshold: 0.5, Axes: []rubric.Axis{
		{Name: "design", Weight: 1},
		{Name: "craft", Weight: 1}, // omitted by the model
	}}
	a, err := rubric.NewModelGrader(m, r).Assess(gradeCtx(), rubric.Subject{Objective: "o", Result: "r"})
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if len(a.Axes) != 2 {
		t.Fatalf("axes = %d, want 2 (unknown axis dropped)", len(a.Axes))
	}
	if a.Axes[1].Scored {
		t.Fatalf("omitted craft axis should be unscored")
	}
	// (1*1.0 + 1*0.0)/2 = 0.5.
	if !approx(a.Overall, 0.5) {
		t.Fatalf("overall = %v, want 0.5", a.Overall)
	}
}

// scriptedGrader is a Grader that returns a fixed assessment, for exercising Regrade and
// callers without a model.
type scriptedGrader struct {
	a   rubric.Assessment
	err error
}

func (s scriptedGrader) Assess(context.Context, rubric.Subject) (rubric.Assessment, error) {
	return s.a, s.err
}

var _ rubric.Grader = scriptedGrader{}
