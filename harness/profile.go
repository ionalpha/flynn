// Package harness decides how much scaffolding the agent loop applies to a given model. A
// strong model is driven leanly; a weaker or more heavily quantized one is given more help,
// such as grammar-constrained decoding, a tighter context, and extra verification, so it can
// still complete a task reliably. The decision is a pure function of a model's measured
// capability, so the policy is testable on its own; the assembly that builds the model client
// and executor applies the resulting plan, and a separate evaluation produces the profile.
package harness

// ModelProfile is a model's measured capability fingerprint, for a particular model, quant, and
// runtime. Each score is in [0,1] where higher is more capable. The zero value is the unknown,
// worst case on purpose: an unmeasured model is treated conservatively and given the most help.
type ModelProfile struct {
	// ToolCallReliability is how reliably the model emits well-formed tool calls.
	ToolCallReliability float64
	// StructuredOutput is how reliably the model adheres to a required output shape.
	StructuredOutput float64
	// InstructionFollowing is how well the model follows long, detailed instructions.
	InstructionFollowing float64
	// EffectiveContext is the token span the model stays coherent over, which is often shorter
	// than its advertised window. Zero means unknown, and imposes no extra cap.
	EffectiveContext int
}

// Plan is how hard the loop works for a model: the adaptations the assembly applies.
type Plan struct {
	// ConstrainToolCalls forces grammar-constrained decoding so a tool call cannot be malformed.
	ConstrainToolCalls bool
	// SimplifyToolSchemas presents smaller, flatter tool schemas a weaker model can follow.
	SimplifyToolSchemas bool
	// MaxContext caps the context window to the model's effective range; zero means no cap.
	MaxContext int
	// VerifyPasses is the number of extra self-check or repair passes before a result is trusted.
	VerifyPasses int
}

// Capability thresholds. Below each, the corresponding scaffolding is applied. They are set so
// that only a model that is reliable on a dimension is driven without help on it.
const (
	// constrainThreshold is the tool-call and structured-output reliability below which decoding
	// is grammar-constrained, since the model cannot be trusted to stay well-formed on its own.
	constrainThreshold = 0.9
	// simplifyThreshold is the instruction-following score below which tool schemas are
	// simplified, since the model struggles with detailed, nested definitions.
	simplifyThreshold = 0.7
	// verifyStrong and verifyShaky bound the verification ramp: at or above verifyStrong a
	// result is trusted as is, below verifyShaky it gets two passes, and between, one.
	verifyStrong = 0.9
	verifyShaky  = 0.7
)

// Adapt maps a profile to a plan. Weaker capability yields more scaffolding and never less: low
// tool-call or structured-output reliability forces constrained decoding; weak
// instruction-following simplifies schemas; a known-narrow effective context caps the window;
// and lower overall reliability adds verification passes. The mapping is monotonic, so a
// strictly weaker model is never given less help than a stronger one, and the zero-value
// profile yields the most conservative plan.
func Adapt(p ModelProfile, advertisedContext int) Plan {
	return Plan{
		ConstrainToolCalls:  p.ToolCallReliability < constrainThreshold || p.StructuredOutput < constrainThreshold,
		SimplifyToolSchemas: p.InstructionFollowing < simplifyThreshold,
		MaxContext:          effectiveCap(p.EffectiveContext, advertisedContext),
		VerifyPasses:        verifyPasses(min(p.ToolCallReliability, p.StructuredOutput, p.InstructionFollowing)),
	}
}

// effectiveCap is the context cap for a model: the smaller of its effective and advertised
// windows, treating an unknown (non-positive) value on either side as no constraint from that
// side. An unknown effective context imposes no extra cap; the other levers carry the caution.
func effectiveCap(effective, advertised int) int {
	switch {
	case effective <= 0 && advertised <= 0:
		return 0 // neither side constrains
	case effective <= 0:
		return advertised
	case advertised <= 0:
		return effective
	default:
		return min(effective, advertised)
	}
}

// verifyPasses ramps verification with the model's weakest measured dimension: a reliable model
// is trusted as is, a shaky one gets one pass, and a poor one gets two.
func verifyPasses(weakest float64) int {
	switch {
	case weakest >= verifyStrong:
		return 0
	case weakest >= verifyShaky:
		return 1
	default:
		return 2
	}
}
