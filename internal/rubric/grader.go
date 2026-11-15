package rubric

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// Subject is what the grader judges: the objective, the result, and Evidence, the
// ground-truth record the grader rules over (a diff, a tool digest, a rendered view).
// The grader judges only what Evidence carries. A visual result described only in
// prose cannot be judged by any rubric; the fix is richer evidence upstream.
type Subject struct {
	Objective string
	Result    string
	Evidence  string
}

// Grader turns a subject into a multi-axis assessment. The production grader is
// [ModelGrader]; a test supplies a scripted one.
type Grader interface {
	Assess(ctx context.Context, s Subject) (Assessment, error)
}

// defaultGradeSystem frames the model as a strict grader that confesses first: name
// every problem, then score, and never let a low-severity framing raise a score. It
// asks for one strict JSON object so the reply parses deterministically. An ungoverned
// judge notices a real issue and approves the work anyway; this forbids that.
const defaultGradeSystem = `You grade finished work against a rubric. You are strict and specific.

Work in two steps, in this order:
1. CONFESS. List every issue you can find in the work — correctness gaps, rough edges, missing pieces, anything below the bar. Be exhaustive. Do NOT decide an issue "isn't a big deal" and leave it out; record it and let the scores reflect it.
2. SCORE. Only then, score each axis on the rubric's scale using the calibration examples as your anchor. An axis's score must be consistent with the issues you confessed: if you found real problems on an axis, its score cannot be near the top.

Judge only what the evidence actually shows. If the evidence cannot support a judgment on an axis, say so in that axis's reason and score it low rather than assuming the best.

Return ONLY a JSON object (no prose, no code fence):
{"issues":[{"axis":string,"severity":string,"detail":string}],"scores":[{"axis":string,"score":number,"reason":string}]}
Score every axis named in the rubric. "axis" in issues is the axis it bears on, or "" if general.`

// ModelGrader is a [Grader] backed by a language model: it renders the rubric and
// subject into one request, parses the strict JSON verdict, and scores it with
// [Rubric.Assemble], so the scoring and threshold logic stay model-free and testable.
type ModelGrader struct {
	model     llm.Model
	rubric    Rubric
	system    string
	maxTokens int
}

// GraderOption configures a ModelGrader.
type GraderOption func(*ModelGrader)

// WithGraderSystem overrides the standing instruction framing the grade. A replacement
// must keep the confess-first rule, or scores drift back toward optimism.
func WithGraderSystem(s string) GraderOption {
	return func(g *ModelGrader) {
		if strings.TrimSpace(s) != "" {
			g.system = s
		}
	}
}

// WithGraderMaxTokens caps the output length requested of the model.
func WithGraderMaxTokens(n int) GraderOption {
	return func(g *ModelGrader) {
		if n > 0 {
			g.maxTokens = n
		}
	}
}

// NewModelGrader builds a model-backed grader over m that judges against r.
func NewModelGrader(m llm.Model, r Rubric, opts ...GraderOption) *ModelGrader {
	g := &ModelGrader{model: m, rubric: r, system: defaultGradeSystem, maxTokens: 1024}
	for _, o := range opts {
		o(g)
	}
	return g
}

var _ Grader = (*ModelGrader)(nil)

// Assess renders the subject into the grading request, asks the model, and assembles
// the verdict. A reply with no JSON object is a terminal fault, so a broken grader is
// visible instead of read as a pass. Scores for axes the rubric does not name are
// ignored; an axis the model omits gets zero credit from Assemble.
func (g *ModelGrader) Assess(ctx context.Context, s Subject) (Assessment, error) {
	resp, err := g.model.Generate(ctx, llm.Request{
		System:    g.system,
		Messages:  []llm.Message{llm.Text(llm.RoleUser, g.prompt(s))},
		MaxTokens: g.maxTokens,
	})
	if err != nil {
		return Assessment{}, err
	}
	scores, issues, err := parseVerdict(resp.Message.TextContent())
	if err != nil {
		return Assessment{}, err
	}
	return g.rubric.Assemble(scores, issues), nil
}

// prompt renders the rubric (axes with guides and weights, then the calibration bands
// as worked examples) and then the subject. The bands pin the scale; without them a
// model reinvents what each score means on every call.
func (g *ModelGrader) prompt(s Subject) string {
	r := g.rubric
	var b strings.Builder
	fmt.Fprintf(&b, "Rubric: %s\nScore each axis from 0 to %g.\n\nAxes:\n", r.Name, r.scaleMax())
	for _, ax := range r.Axes {
		fmt.Fprintf(&b, "- %s (weight %g): %s\n", ax.Name, effectiveWeight(ax.Weight), ax.Guide)
	}
	if len(r.Bands) > 0 {
		b.WriteString("\nCalibration (what a score means — anchor to these):\n")
		for _, band := range r.Bands {
			fmt.Fprintf(&b, "- %s = %g: %s\n", band.Axis, band.Score, band.Reason)
		}
	}
	b.WriteString("\n--- Work under grade ---\n")
	fmt.Fprintf(&b, "Objective:\n%s\n\nResult:\n%s\n", s.Objective, s.Result)
	if strings.TrimSpace(s.Evidence) != "" {
		fmt.Fprintf(&b, "\nEvidence (the ground-truth record; judge only what this shows):\n%s\n", s.Evidence)
	}
	return b.String()
}

type verdictJSON struct {
	Issues []struct {
		Axis     string `json:"axis"`
		Severity string `json:"severity"`
		Detail   string `json:"detail"`
	} `json:"issues"`
	Scores []struct {
		Axis   string  `json:"axis"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	} `json:"scores"`
}

// parseVerdict extracts the JSON object from text into per-axis raw scores and
// confessed issues. Models wrap JSON in prose or code fences despite instructions, so
// it takes the outermost object. A missing or malformed object is a terminal fault.
func parseVerdict(text string) (map[string]RawScore, []Issue, error) {
	raw := extractObject(text)
	if raw == "" {
		return nil, nil, fault.New(fault.Terminal, "grade_parse", "grader returned no JSON object")
	}
	var v verdictJSON
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, nil, fault.Wrap(fault.Terminal, "grade_parse", err)
	}
	scores := make(map[string]RawScore, len(v.Scores))
	for _, s := range v.Scores {
		scores[s.Axis] = RawScore{Score: s.Score, Reason: s.Reason}
	}
	issues := make([]Issue, 0, len(v.Issues))
	for _, is := range v.Issues {
		issues = append(issues, Issue{Axis: is.Axis, Severity: is.Severity, Detail: is.Detail})
	}
	return scores, issues, nil
}

// extractObject returns the outermost JSON object in text (from the first "{" to the
// last "}"), or "" if there is none.
func extractObject(text string) string {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}
