package mission

import "github.com/ionalpha/flynn/harness"

// inputContextNumer and inputContextDenom set the share of a model's effective context window
// the input transcript is allowed to fill before compaction elides older turns, reserving the
// remainder for the model's own reply. A budget that bounds only the input must leave room for
// output, or a model whose window is exactly full has nowhere to answer.
const (
	inputContextNumer = 3
	inputContextDenom = 4
)

// PlanOptions translates a capability-driven scaffolding plan into the executor options that
// apply it: a tightened context budget for a model with a narrow window, simplified tool schemas
// for a weaker instruction-follower, and self-check passes for a less reliable model. The plan's
// tool-call constraint is applied to the model client at construction, not here, so it is not in
// this set. The zero plan yields no options, leaving a strong model on the lean path. Callers
// append the result after their own defaults, so a present plan field overrides a default and an
// absent one leaves it in place.
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
