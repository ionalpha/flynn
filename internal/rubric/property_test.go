package rubric_test

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/rubric"
)

// TestProp_OverallIsBoundedAndConsistent checks the defining invariants of Assemble over
// arbitrary rubrics and judgments: the overall score is always in [0,1] no matter how the
// grader scores (including out-of-range scores, which clamp) or how the axes are weighted
// (including zero and negative weights); the pass/fail verdict is exactly the overall
// against the threshold; and scoring is pure, so the same inputs assemble identically.
func TestProp_OverallIsBoundedAndConsistent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "axes")
		scale := rapid.Float64Range(1, 100).Draw(rt, "scale")
		threshold := rapid.Float64Range(0, 1).Draw(rt, "threshold")

		axes := make([]rubric.Axis, n)
		scores := map[string]rubric.RawScore{}
		for i := range axes {
			name := fmt.Sprintf("ax%d", i)
			axes[i] = rubric.Axis{Name: name, Weight: rapid.Float64Range(-2, 5).Draw(rt, "weight")}
			if rapid.Bool().Draw(rt, "scored") {
				// Deliberately allow scores outside [0,scale]: clamp must absorb them.
				scores[name] = rubric.RawScore{Score: rapid.Float64Range(-10, scale+10).Draw(rt, "score")}
			}
		}
		r := rubric.Rubric{Name: "prop", Max: scale, Threshold: threshold, Axes: axes}

		a := r.Assemble(scores, nil)
		if a.Overall < 0 || a.Overall > 1 {
			rt.Fatalf("overall %v out of [0,1]", a.Overall)
		}
		if a.Passed != (a.Overall >= threshold) {
			rt.Fatalf("passed=%v but overall=%v vs threshold=%v", a.Passed, a.Overall, threshold)
		}
		if len(a.Axes) != n {
			rt.Fatalf("emitted %d axes, want %d (one per rubric axis, in order)", len(a.Axes), n)
		}
		// Pure: re-assembling the same inputs gives the identical score.
		if b := r.Assemble(scores, nil); b.Overall != a.Overall || b.Passed != a.Passed {
			rt.Fatalf("assemble not deterministic: %v/%v vs %v/%v", a.Overall, a.Passed, b.Overall, b.Passed)
		}
	})
}

// TestProp_RaisingAScoreNeverLowersOverall checks monotonicity: scoring an axis higher
// (up to the scale top) cannot decrease the overall. A grader that judges one axis more
// generously can only move the grade up, never paradoxically down.
func TestProp_RaisingAScoreNeverLowersOverall(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(rt, "axes")
		const scale = 10.0
		axes := make([]rubric.Axis, n)
		low := map[string]rubric.RawScore{}
		high := map[string]rubric.RawScore{}
		for i := range axes {
			name := fmt.Sprintf("ax%d", i)
			axes[i] = rubric.Axis{Name: name, Weight: rapid.Float64Range(0, 5).Draw(rt, "weight")}
			lo := rapid.Float64Range(0, scale).Draw(rt, "lo")
			bump := rapid.Float64Range(0, scale-lo).Draw(rt, "bump")
			low[name] = rubric.RawScore{Score: lo}
			high[name] = rubric.RawScore{Score: lo + bump}
		}
		r := rubric.Rubric{Name: "mono", Max: scale, Axes: axes}
		lo := r.Assemble(low, nil).Overall
		hi := r.Assemble(high, nil).Overall
		if hi+1e-9 < lo {
			rt.Fatalf("raising scores lowered overall: %v -> %v", lo, hi)
		}
	})
}
