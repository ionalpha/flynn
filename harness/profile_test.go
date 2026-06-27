package harness

import "testing"

func TestAdaptZeroValueIsMostConservative(t *testing.T) {
	plan := Adapt(ModelProfile{}, 8192)
	if !plan.ConstrainToolCalls {
		t.Error("an unmeasured model must have its tool calls constrained")
	}
	if !plan.SimplifyToolSchemas {
		t.Error("an unmeasured model must get simplified schemas")
	}
	if plan.VerifyPasses != 2 {
		t.Errorf("VerifyPasses = %d, want the maximum 2 for an unmeasured model", plan.VerifyPasses)
	}
}

func TestAdaptStrongModelRunsLean(t *testing.T) {
	strong := ModelProfile{ToolCallReliability: 1, StructuredOutput: 1, InstructionFollowing: 1, EffectiveContext: 0}
	plan := Adapt(strong, 8192)
	if plan.ConstrainToolCalls || plan.SimplifyToolSchemas || plan.VerifyPasses != 0 {
		t.Fatalf("a fully reliable model must run unscaffolded, got %+v", plan)
	}
	if plan.MaxContext != 8192 {
		t.Errorf("with unknown effective context the cap is the advertised window, got %d", plan.MaxContext)
	}
}

func TestAdaptThresholds(t *testing.T) {
	// Just reliable enough on every dimension: no scaffolding.
	good := ModelProfile{ToolCallReliability: 0.9, StructuredOutput: 0.9, InstructionFollowing: 0.7}
	if p := Adapt(good, 0); p.ConstrainToolCalls || p.SimplifyToolSchemas || p.VerifyPasses != 1 {
		// instruction 0.7 is at the simplify edge (not simplified) but is the weakest dim, so it
		// earns one verify pass.
		t.Fatalf("threshold-edge model = %+v, want no constrain/simplify and one verify pass", p)
	}
	// A dip in tool-call reliability alone forces constrained decoding.
	if p := Adapt(ModelProfile{ToolCallReliability: 0.85, StructuredOutput: 1, InstructionFollowing: 1}, 0); !p.ConstrainToolCalls {
		t.Error("low tool-call reliability must force constrained decoding")
	}
	// Weak instruction-following alone simplifies schemas.
	if p := Adapt(ModelProfile{ToolCallReliability: 1, StructuredOutput: 1, InstructionFollowing: 0.6}, 0); !p.SimplifyToolSchemas {
		t.Error("weak instruction following must simplify schemas")
	}
}

func TestEffectiveCap(t *testing.T) {
	cases := []struct {
		effective, advertised, want int
	}{
		{0, 0, 0},
		{0, 4096, 4096},
		{4096, 0, 4096},
		{2048, 8192, 2048}, // narrow effective context tightens the cap
		{8192, 4096, 4096}, // no benefit capping above the advertised window
		{-5, -5, 0},        // malformed input never yields a negative cap
	}
	for _, c := range cases {
		if got := effectiveCap(c.effective, c.advertised); got != c.want {
			t.Errorf("effectiveCap(%d,%d) = %d, want %d", c.effective, c.advertised, got, c.want)
		}
	}
}
