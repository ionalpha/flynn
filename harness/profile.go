// Package harness decides how much scaffolding the agent loop gives a model. A
// strong model runs lean; a weaker or heavily quantized one gets constrained
// decoding, a tighter context and extra verification. The decision is a pure
// function of measured capability, so the policy is testable alone.
package harness

// ModelProfile is a measured capability fingerprint for one model, quant and
// runtime. Scores are in [0,1], higher more capable. The zero value is the worst
// case, so an unmeasured model gets the most help.
type ModelProfile struct {
	// ToolCallReliability is how reliably the model emits well-formed tool calls.
	ToolCallReliability float64
	// StructuredOutput is how reliably the model adheres to a required output shape.
	StructuredOutput float64
	// InstructionFollowing is how well the model follows long, detailed instructions.
	InstructionFollowing float64
	// EffectiveContext is the token span the model stays coherent over, often shorter
	// than its advertised window. Zero means unknown and imposes no cap.
	EffectiveContext int
}

// Plan is the scaffolding the assembly applies for a model.
type Plan struct {
	// ConstrainToolCalls forces grammar-constrained decoding so a tool call cannot be malformed.
	ConstrainToolCalls bool
	// SimplifyToolSchemas presents smaller, flatter tool schemas.
	SimplifyToolSchemas bool
	// MaxContext caps the context window; zero means no cap.
	MaxContext int
	// VerifyPasses is the number of self-check or repair passes before a result is trusted.
	VerifyPasses int
}

// Capability thresholds. Below each, its scaffolding is applied.
const (
	// constrainThreshold is the tool-call and structured-output reliability below
	// which decoding is grammar-constrained.
	constrainThreshold = 0.9
	// simplifyThreshold is the instruction-following score below which schemas are
	// simplified.
	simplifyThreshold = 0.7
	// verifyStrong and verifyShaky bound the verification ramp: at or above
	// verifyStrong no pass, below verifyShaky two, between them one.
	verifyStrong = 0.9
	verifyShaky  = 0.7
)

// Adapt maps a profile to a plan. Low tool-call or structured-output reliability
// forces constrained decoding, weak instruction-following simplifies schemas, a
// narrow effective context caps the window, and lower reliability adds verify
// passes. The mapping is monotonic, so a weaker model never gets less help, and the
// zero profile gets the most.
func Adapt(p ModelProfile, advertisedContext int) Plan {
	return Plan{
		ConstrainToolCalls:  p.ToolCallReliability < constrainThreshold || p.StructuredOutput < constrainThreshold,
		SimplifyToolSchemas: p.InstructionFollowing < simplifyThreshold,
		MaxContext:          effectiveCap(p.EffectiveContext, advertisedContext),
		VerifyPasses:        verifyPasses(min(p.ToolCallReliability, p.StructuredOutput, p.InstructionFollowing)),
	}
}

// effectiveCap is the smaller of the effective and advertised windows, a
// non-positive side imposing no constraint.
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

// verifyPasses ramps verification with the weakest measured dimension.
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
