package harness

import "testing"

// TestPlanForUnknownIsConservative proves the safe default is automatic: a nil source, and a
// source that has not measured the model, both yield the fully scaffolded plan, so a model is
// never driven leanly on assumption.
func TestPlanForUnknownIsConservative(t *testing.T) {
	conservative := Adapt(ModelProfile{}, 8000)
	if got := PlanFor(nil, "anything", 8000); got != conservative {
		t.Fatalf("nil source plan = %+v, want the conservative %+v", got, conservative)
	}
	empty := StaticProfiles{}
	if got := PlanFor(empty, "unmeasured", 8000); got != conservative {
		t.Fatalf("unmeasured plan = %+v, want the conservative %+v", got, conservative)
	}
}

// TestPlanForMeasuredUsesProfile proves a measured, reliable model is driven leanly: its recorded
// profile maps to a plan with no scaffolding, distinct from the conservative default.
func TestPlanForMeasuredUsesProfile(t *testing.T) {
	strong := ModelProfile{
		ToolCallReliability:  1,
		StructuredOutput:     1,
		InstructionFollowing: 1,
	}
	src := StaticProfiles{"good:model": strong}
	got := PlanFor(src, "good:model", 8000)
	if got.ConstrainToolCalls || got.SimplifyToolSchemas || got.VerifyPasses != 0 {
		t.Fatalf("a measured-strong model must be driven leanly, got %+v", got)
	}
	if got != Adapt(strong, 8000) {
		t.Fatalf("PlanFor did not map the measured profile through Adapt: %+v", got)
	}
}

// TestPlanForCarriesAdvertisedContext proves the advertised window flows into the plan's context
// cap for an unknown model (which has no measured effective window of its own).
func TestPlanForCarriesAdvertisedContext(t *testing.T) {
	if got := PlanFor(nil, "x", 4096); got.MaxContext != 4096 {
		t.Fatalf("MaxContext = %d, want the advertised 4096", got.MaxContext)
	}
}
