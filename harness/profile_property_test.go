package harness

import (
	"testing"

	"pgregory.net/rapid"
)

// TestAdaptIsMonotonic asserts the core safety property: a weaker model is never given less
// scaffolding than a stronger one. For two profiles where every capability score of the weak
// one is at most that of the strong one, the weak plan must keep constrained decoding and
// schema simplification wherever the strong plan has them, and use at least as many verify
// passes.
func TestAdaptIsMonotonic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		score := func(label string) float64 { return rapid.Float64Range(0, 1).Draw(rt, label) }
		strong := ModelProfile{
			ToolCallReliability:  score("tcr"),
			StructuredOutput:     score("so"),
			InstructionFollowing: score("if"),
		}
		drop := func(v float64, label string) float64 {
			r := v - rapid.Float64Range(0, 1).Draw(rt, label)
			if r < 0 {
				r = 0
			}
			return r
		}
		weak := ModelProfile{
			ToolCallReliability:  drop(strong.ToolCallReliability, "dtcr"),
			StructuredOutput:     drop(strong.StructuredOutput, "dso"),
			InstructionFollowing: drop(strong.InstructionFollowing, "dif"),
		}
		adv := rapid.IntRange(0, 1_000_000).Draw(rt, "adv")
		ps, pw := Adapt(strong, adv), Adapt(weak, adv)

		if ps.ConstrainToolCalls && !pw.ConstrainToolCalls {
			rt.Fatal("a weaker model lost constrained decoding")
		}
		if ps.SimplifyToolSchemas && !pw.SimplifyToolSchemas {
			rt.Fatal("a weaker model lost schema simplification")
		}
		if pw.VerifyPasses < ps.VerifyPasses {
			rt.Fatalf("a weaker model got fewer verify passes: %d < %d", pw.VerifyPasses, ps.VerifyPasses)
		}
	})
}

// TestAdaptContextCapInvariant asserts the context cap never exceeds a known window and is never
// negative, for any effective and advertised sizes.
func TestAdaptContextCapInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		eff := rapid.IntRange(0, 1_000_000).Draw(rt, "eff")
		adv := rapid.IntRange(0, 1_000_000).Draw(rt, "adv")
		maxCtx := Adapt(ModelProfile{EffectiveContext: eff}, adv).MaxContext
		if maxCtx < 0 {
			rt.Fatalf("negative context cap: %d", maxCtx)
		}
		if eff > 0 && maxCtx > eff {
			rt.Fatalf("cap %d exceeds the effective window %d", maxCtx, eff)
		}
		if adv > 0 && maxCtx > adv {
			rt.Fatalf("cap %d exceeds the advertised window %d", maxCtx, adv)
		}
	})
}
