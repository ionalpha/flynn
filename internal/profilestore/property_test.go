package profilestore

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/harness"
)

// drawScore draws a reliability score within the schema's [0,1] range.
func drawScore(rt *rapid.T, label string) float64 {
	return rapid.Float64Range(0, 1).Draw(rt, label)
}

// TestProfileRoundTripsAndFoldsMinimum proves two properties of the store over arbitrary inputs:
// a single profile read back is exactly what was written, and several profiles for one model fold
// to the element-wise minimum. Both matter because the value drives how a model is scaffolded, so
// a lossy round-trip or an over-generous fold would mis-drive the harness.
func TestProfileRoundTripsAndFoldsMinimum(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		rs := newStore(t)
		n := rapid.IntRange(1, 4).Draw(rt, "writes")
		var want harness.ModelProfile
		for i := range n {
			spec := Spec{
				ModelID: "m",
				// A distinct quant per write, so each is its own stored target and the read folds
				// across all of them rather than an earlier write being overwritten.
				Quant:                fmt.Sprintf("q%d", i),
				ToolCallReliability:  drawScore(rt, "tcr"),
				StructuredOutput:     drawScore(rt, "so"),
				InstructionFollowing: drawScore(rt, "if"),
				EffectiveContext:     rapid.IntRange(0, 32768).Draw(rt, "ctx"),
			}
			if err := Write(t.Context(), rs, spec); err != nil {
				rt.Fatalf("write: %v", err)
			}
			if i == 0 {
				want = spec.profile()
			} else {
				want = mostConservative(want, spec.profile())
			}
		}

		src, err := NewSource(t.Context(), rs)
		if err != nil {
			rt.Fatal(err)
		}
		got, ok := src.Profile("m")
		if !ok {
			rt.Fatal("profile absent after writes")
		}
		if got != want {
			rt.Fatalf("read profile = %+v, want folded %+v", got, want)
		}
		// The mapped scores must always stay within the schema's range.
		for _, s := range []float64{got.ToolCallReliability, got.StructuredOutput, got.InstructionFollowing} {
			if s < 0 || s > 1 {
				rt.Fatalf("score %v left [0,1]", s)
			}
		}
	})
}
