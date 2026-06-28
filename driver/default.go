package driver

import (
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
)

// NameDefault is the blessed general-purpose loop: the tool-using conversation that
// advances a goal by calling the model, running the tools it asks for, feeding the
// results back, and converging when the model finishes. It is the zero-config
// default, so a run that names no driver gets it.
const NameDefault = "general-software"

// defaultDriver builds the general-purpose loop. It is the mission executor wired
// from the Spec, so behaviour is identical to assembling that loop directly; the
// indirection only makes the choice nameable and swappable.
type defaultDriver struct{}

// Name identifies the default driver.
func (defaultDriver) Name() string { return NameDefault }

// Build assembles the mission loop from the Spec. Each ingredient maps to the
// option that wires it, so an absent ingredient (no tools, no grant, no sandbox)
// simply leaves that lever at its zero-config default and behaviour is unchanged.
// The capability-scaffolding Plan is applied through the options it maps cleanly
// onto; the levers without a loop-level option (grammar-constrained decoding, the
// effective-context cap) are applied where the model client is built, not here.
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
	if s.Fanout != nil {
		opts = append(opts, mission.WithFanout(s.Fanout))
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
