package driver

import (
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
)

// NameDefault is the general-purpose tool-using conversation loop, used when a run names
// no driver.
const NameDefault = "general-software"

// defaultDriver builds the mission executor from the Spec.
type defaultDriver struct{}

// Name returns [NameDefault].
func (defaultDriver) Name() string { return NameDefault }

// Build maps each Spec ingredient to its mission option. The Plan levers with no loop
// option (grammar-constrained decoding, the effective-context cap) are applied where the
// model client is built.
func (defaultDriver) Build(s Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	opts := []mission.Option{mission.WithSystem(s.System)}
	if len(s.Tools) > 0 {
		opts = append(opts, mission.WithTools(s.Tools...))
	}
	if s.HasGrant {
		opts = append(opts, mission.WithGrant(s.Grant))
	}
	if s.Sandbox != nil {
		opts = append(opts, mission.WithSandbox(s.Sandbox))
	}
	if s.Reporter != nil {
		opts = append(opts, mission.WithObserver(s.Reporter))
	}
	if s.Brakes != nil {
		opts = append(opts, mission.WithBrakes(s.Brakes))
	}
	if s.Budget != nil {
		opts = append(opts, mission.WithBudget(s.Budget))
	}
	if s.Fanout != nil {
		opts = append(opts, mission.WithFanout(s.Fanout))
	}
	if s.Approval != nil {
		opts = append(opts, mission.WithApproval(s.Approval.Gate))
		opts = append(opts, mission.WithApprovalPrompter(s.Approval.Prompter, s.Approval.Signer, s.Approval.Host))
	}
	if s.Allowance != nil {
		opts = append(opts, mission.WithAllowance(s.Allowance))
	}
	if s.EventSink != nil {
		opts = append(opts, mission.WithEventSink(s.EventSink))
	}
	if s.CompactionBudget > 0 {
		opts = append(opts, mission.WithCompactionBudget(s.CompactionBudget))
	}
	if s.Plan.SimplifyToolSchemas {
		opts = append(opts, mission.WithSimplifiedSchemas())
	}
	if s.Plan.VerifyPasses > 0 {
		opts = append(opts, mission.WithVerifyPasses(s.Plan.VerifyPasses))
	}
	return mission.NewExecutor(s.Model, opts...), mission.Convergence{}, nil
}

var _ Driver = defaultDriver{}
