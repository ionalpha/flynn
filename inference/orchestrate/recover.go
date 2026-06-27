package orchestrate

// This file is the resilience counterpart to the scheduling policy: a pure decision that
// says how to react when a model server keeps failing to start or stay healthy. It is kept
// free of any runtime or serve dependency so the recovery ladder can be reasoned about and
// tested exhaustively; the controller that observes failures and applies the chosen action,
// and the serve layer that classifies a runtime's failure, are wired separately.

// FailureKind classifies why a model server failed, because the right reaction differs by
// cause: retrying an out-of-memory launch unchanged is futile, while a one-off crash is
// worth another try.
type FailureKind int

const (
	// FailureNone means the model is not in a failed state.
	FailureNone FailureKind = iota
	// FailureCrash means the server process exited or failed to start, with no sign that
	// memory was the cause: a transient fault worth retrying a bounded number of times.
	FailureCrash
	// FailureOOM means the launch ran out of device or host memory. Retrying the same
	// footprint cannot succeed, so recovery must shrink it or fall back.
	FailureOOM
	// FailureHang means the server started but never became healthy: it may be a slow load
	// worth one retry, or a wedged runtime that needs a lighter config or a fallback.
	FailureHang
)

// RecoveryAction is the next step to take for a failing model, in increasing severity. The
// controller maps each onto a concrete move: retry the same launch, relaunch with a reduced
// footprint, serve a smaller fallback model instead, or stop trying for now.
type RecoveryAction int

const (
	// RecoverRetry tries the same launch again; the failure looked transient.
	RecoverRetry RecoveryAction = iota
	// RecoverDegrade relaunches with a smaller footprint (shorter context, lower memory
	// use), the first answer to memory pressure or a wedged load.
	RecoverDegrade
	// RecoverFallback gives up on this model for now and serves a smaller one in its place,
	// so something usable stays available rather than nothing.
	RecoverFallback
	// RecoverQuarantine stops launching the model until its state is reset, so a model that
	// fails no matter what does not consume the loop in a hot restart cycle.
	RecoverQuarantine
)

// RecoveryState is a model's recent failure memory: the kind of the most recent failure and
// how many consecutive attempts have failed (including the one that produced this state). A
// successful launch resets it to the zero value.
type RecoveryState struct {
	Kind     FailureKind
	Attempts int
}

// Recover decides the next action for a failing model, applying a graded ladder that escalates
// with repeated failure and never retries a cause that an identical retry cannot fix. An
// out-of-memory failure degrades, then falls back, then quarantines, and is never retried
// unchanged. A hang gets one retry (a slow load) before degrading and falling back. A plain
// crash is retried a bounded number of times, then quarantined. The action severity never
// decreases as attempts grow, so escalation is monotonic and a persistently failing model
// always reaches quarantine rather than looping forever.
func Recover(s RecoveryState) RecoveryAction {
	attempts := s.Attempts
	if attempts < 1 {
		attempts = 1
	}
	switch s.Kind {
	case FailureOOM:
		switch attempts {
		case 1:
			return RecoverDegrade
		case 2:
			return RecoverFallback
		default:
			return RecoverQuarantine
		}
	case FailureHang:
		switch attempts {
		case 1:
			return RecoverRetry
		case 2:
			return RecoverDegrade
		case 3:
			return RecoverFallback
		default:
			return RecoverQuarantine
		}
	case FailureCrash:
		if attempts <= 2 {
			return RecoverRetry
		}
		return RecoverQuarantine
	default:
		return RecoverRetry
	}
}
