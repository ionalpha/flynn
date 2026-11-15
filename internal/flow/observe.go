package flow

// StepPhase marks whether a StepEvent is reported as a step begins or after it ends.
type StepPhase int

const (
	// StepBegin is reported just before a step's external effect runs.
	StepBegin StepPhase = iota
	// StepEnd is reported after a step's external effect completes, carrying its outcome.
	StepEnd
)

// StepEvent describes one observable step, reported as it begins and as it ends so a
// host can show progress. Only exec and dependency steps are reported; pure steps
// have no external effect to watch.
type StepEvent struct {
	// Phase is StepBegin or StepEnd.
	Phase StepPhase
	// Op is the step's op (OpExec or OpDependency).
	Op Op
	// Detail is the rendered command for exec, the dependency name for dependency.
	Detail string
	// ExitCode is the command's exit code, set on StepEnd for an exec step.
	ExitCode int
	// Output is the command's combined output, set on StepEnd for an exec step.
	Output string
	// Err is set on StepEnd when the step failed.
	Err error
}

// Observer watches a flow step by step, so a host can show progress through a long
// procedure such as provisioning or a deploy. Implementations must not block or
// retain the event past the call.
type Observer interface {
	Step(StepEvent)
}
