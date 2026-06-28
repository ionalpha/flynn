package flow

// StepPhase marks whether a StepEvent is reported as a step's effect begins or after it
// ends.
type StepPhase int

const (
	// StepBegin is reported just before a step's external effect runs.
	StepBegin StepPhase = iota
	// StepEnd is reported after a step's external effect completes, carrying its outcome.
	StepEnd
)

// StepEvent describes one observable step of a running flow. It is reported to an Observer
// as the step begins and again as it ends, so a host can show progress while a flow runs
// instead of only seeing the final result. Only steps with an external effect worth
// watching are reported, the command and dependency steps; pure steps (transform,
// condition, loop, return) are not, because they have nothing to watch.
type StepEvent struct {
	// Phase is whether the step is beginning or has ended.
	Phase StepPhase
	// Op is the step's op (OpExec or OpDependency).
	Op Op
	// Detail is the human-readable subject: the rendered command for an exec step, the
	// dependency name for a dependency step.
	Detail string
	// ExitCode is the command's exit code, set on StepEnd for an exec step.
	ExitCode int
	// Output is the command's combined output, set on StepEnd for an exec step.
	Output string
	// Err is set on StepEnd when the step failed.
	Err error
}

// Observer watches a flow execute, step by step. The interpreter reports each observable
// step as it begins (StepBegin) and after it ends (StepEnd), so a host can show progress
// while a long-running procedure (provisioning a tool, deploying an app) happens rather
// than only seeing the final result. A nil Observer disables reporting. Implementations
// must not block or retain the event past the call.
type Observer interface {
	Step(StepEvent)
}
