package harness

import "testing"

// FuzzAdapt throws arbitrary capability scores and context sizes, including out-of-range and
// non-finite values, at the policy and asserts it never panics and always returns a valid plan:
// a bounded number of verify passes and a non-negative context cap.
func FuzzAdapt(f *testing.F) {
	f.Add(0.0, 0.0, 0.0, 0, 0)
	f.Add(1.0, 1.0, 1.0, 8192, 8192)
	f.Add(-1.0, 2.0, 0.5, -10, 100)

	f.Fuzz(func(t *testing.T, tcr, so, instr float64, eff, adv int) {
		p := Adapt(ModelProfile{
			ToolCallReliability:  tcr,
			StructuredOutput:     so,
			InstructionFollowing: instr,
			EffectiveContext:     eff,
		}, adv)
		if p.VerifyPasses < 0 || p.VerifyPasses > 2 {
			t.Fatalf("VerifyPasses out of range: %d", p.VerifyPasses)
		}
		if p.MaxContext < 0 {
			t.Fatalf("negative MaxContext: %d", p.MaxContext)
		}
	})
}
