package mission

import "github.com/ionalpha/flynn/harness"

// inputContextNumer and inputContextDenom cap the input at three quarters of the window,
// so a model always has room to answer.
const (
	inputContextNumer = 3
	inputContextDenom = 4
)

// PlanOptions turns a scaffolding plan into executor options; the zero plan yields none.
// The tool-call constraint is applied to the model client instead. Append the result after
// your defaults so a set plan field overrides them.
func PlanOptions(plan harness.Plan) []Option {
	var opts []Option
	if plan.MaxContext > 0 {
		opts = append(opts, WithCompactionBudget(plan.MaxContext*inputContextNumer/inputContextDenom))
	}
	if plan.SimplifyToolSchemas {
		opts = append(opts, WithSimplifiedSchemas())
	}
	if plan.VerifyPasses > 0 {
		opts = append(opts, WithVerifyPasses(plan.VerifyPasses))
	}
	return opts
}
