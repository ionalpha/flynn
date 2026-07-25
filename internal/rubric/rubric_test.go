package rubric_test

import (
	"math"
	"testing"

	"github.com/ionalpha/flynn/internal/rubric"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// twoAxis is a small rubric on a 0..10 scale: one axis weighted 3, one weighted 1.
func twoAxis() rubric.Rubric {
	return rubric.Rubric{
		Name:      "two",
		Max:       10,
		Threshold: 0.6,
		Axes: []rubric.Axis{
			{Name: "big", Weight: 3},
			{Name: "small", Weight: 1},
		},
	}
}

func TestAssembleWeightsAxes(t *testing.T) {
	r := twoAxis()
	// big=8/10, small=4/10. weighted mean = (3*0.8 + 1*0.4) / 4 = 2.8/4 = 0.7.
	a := r.Assemble(map[string]rubric.RawScore{
		"big":   {Score: 8, Reason: "mostly there"},
		"small": {Score: 4, Reason: "thin"},
	}, nil)
	if !approx(a.Overall, 0.7) {
		t.Fatalf("overall = %v, want 0.7", a.Overall)
	}
	if !a.Passed {
		t.Fatalf("passed = false, want true (0.7 >= 0.6)")
	}
	if len(a.Axes) != 2 || a.Axes[0].Axis != "big" || a.Axes[1].Axis != "small" {
		t.Fatalf("axes not in rubric order: %+v", a.Axes)
	}
	if !a.Axes[0].Scored || a.Axes[0].Reason != "mostly there" {
		t.Fatalf("axis reason/scored lost: %+v", a.Axes[0])
	}
}

func TestReweightingChangesTheVerdict(t *testing.T) {
	// Same scores, but flip the weights so the weak axis dominates: the pass becomes a
	// fail. This is the "weights are a tuning knob" property — no prompt involved.
	scores := map[string]rubric.RawScore{"big": {Score: 8}, "small": {Score: 4}}
	heavy := twoAxis() // big weighted 3
	light := twoAxis()
	light.Axes[0].Weight = 1
	light.Axes[1].Weight = 3 // small weighted 3

	if h := heavy.Assemble(scores, nil); !h.Passed {
		t.Fatalf("heavy-on-strong should pass, overall=%v", h.Overall)
	}
	l := light.Assemble(scores, nil)
	// (1*0.8 + 3*0.4)/4 = 2.0/4 = 0.5 < 0.6.
	if l.Passed || !approx(l.Overall, 0.5) {
		t.Fatalf("heavy-on-weak: overall=%v passed=%v, want 0.5 fail", l.Overall, l.Passed)
	}
}

func TestUnscoredAxisIsZeroCreditNotSkipped(t *testing.T) {
	r := twoAxis()
	// Only "big" scored. "small" is unscored: it counts its weight against the total
	// with zero credit, so the grade is dragged down rather than the axis dropped.
	a := r.Assemble(map[string]rubric.RawScore{"big": {Score: 10}}, nil)
	// (3*1.0 + 1*0.0)/4 = 0.75.
	if !approx(a.Overall, 0.75) {
		t.Fatalf("overall = %v, want 0.75", a.Overall)
	}
	if a.Axes[1].Scored {
		t.Fatalf("unscored axis marked scored: %+v", a.Axes[1])
	}
}

func TestScoresClampToScale(t *testing.T) {
	r := twoAxis()
	// A grader that overshoots (12 on a 0..10 axis) or undershoots (-3) is corrected,
	// so a bad number cannot push the weighted total past its bounds.
	a := r.Assemble(map[string]rubric.RawScore{
		"big":   {Score: 12},
		"small": {Score: -3},
	}, nil)
	// clamp -> big=10 (1.0), small=0 (0.0): (3*1 + 1*0)/4 = 0.75.
	if !approx(a.Overall, 0.75) {
		t.Fatalf("overall = %v, want 0.75", a.Overall)
	}
	if a.Overall < 0 || a.Overall > 1 {
		t.Fatalf("overall out of [0,1]: %v", a.Overall)
	}
}

func TestZeroWeightDefaultsToOne(t *testing.T) {
	r := rubric.Rubric{Name: "z", Max: 10, Axes: []rubric.Axis{{Name: "a"}, {Name: "b"}}}
	a := r.Assemble(map[string]rubric.RawScore{"a": {Score: 10}, "b": {Score: 0}}, nil)
	// Both default to weight 1: (1*1 + 1*0)/2 = 0.5.
	if !approx(a.Overall, 0.5) {
		t.Fatalf("overall = %v, want 0.5", a.Overall)
	}
}

func TestNegativeWeightIsZero(t *testing.T) {
	r := rubric.Rubric{Name: "n", Max: 10, Axes: []rubric.Axis{
		{Name: "a", Weight: 1},
		{Name: "b", Weight: -5}, // floored to 0: contributes nothing
	}}
	a := r.Assemble(map[string]rubric.RawScore{"a": {Score: 5}, "b": {Score: 10}}, nil)
	// b weighs 0, so only a counts: 0.5.
	if !approx(a.Overall, 0.5) {
		t.Fatalf("overall = %v, want 0.5", a.Overall)
	}
	if a.Axes[1].Weight != 0 {
		t.Fatalf("negative weight not floored: %v", a.Axes[1].Weight)
	}
}

func TestEmptyRubricScoresZero(t *testing.T) {
	r := rubric.Rubric{Name: "empty"}
	a := r.Assemble(nil, nil)
	if a.Overall != 0 {
		t.Fatalf("overall = %v, want 0", a.Overall)
	}
}

func TestAssembleCarriesIssuesAndFingerprint(t *testing.T) {
	r := twoAxis()
	issues := []rubric.Issue{{Axis: "big", Severity: "major", Detail: "broken"}}
	a := r.Assemble(map[string]rubric.RawScore{"big": {Score: 5}}, issues)
	if len(a.Issues) != 1 || a.Issues[0].Detail != "broken" {
		t.Fatalf("issues lost: %+v", a.Issues)
	}
	if a.Fingerprint == "" || a.Fingerprint != r.Fingerprint() {
		t.Fatalf("fingerprint = %q, want %q", a.Fingerprint, r.Fingerprint())
	}
}

func TestFingerprintChangesWithContent(t *testing.T) {
	base := twoAxis()
	reweighted := twoAxis()
	reweighted.Axes[0].Weight = 2
	if base.Fingerprint() == reweighted.Fingerprint() {
		t.Fatalf("fingerprint did not change when a weight changed")
	}
	// Identical rubrics fingerprint identically.
	if base.Fingerprint() != twoAxis().Fingerprint() {
		t.Fatalf("identical rubrics fingerprint differently")
	}
}

func TestThresholdBoundaryIsInclusive(t *testing.T) {
	r := rubric.Rubric{Name: "t", Max: 10, Threshold: 0.5, Axes: []rubric.Axis{{Name: "a"}}}
	a := r.Assemble(map[string]rubric.RawScore{"a": {Score: 5}}, nil) // exactly 0.5
	if !a.Passed {
		t.Fatalf("overall %v at threshold 0.5 should pass", a.Overall)
	}
}
