package orchestrate

// FailureKind classifies why a model server failed, because the right reaction differs
// by cause: an out-of-memory launch retried unchanged fails again, while a one-off
// crash is worth another try.
type FailureKind int

const (
	// FailureNone means the model is not in a failed state.
	FailureNone FailureKind = iota
	// FailureCrash means the server exited or failed to start with no sign of a memory
	// cause, so a bounded number of retries is worthwhile.
	FailureCrash
	// FailureOOM means the launch ran out of device or host memory, so the footprint must
	// shrink or the model fall back.
	FailureOOM
	// FailureHang means the server started but never became healthy: a slow load worth
	// one retry, or a wedged runtime that needs a lighter config.
	FailureHang
)

// RecoveryAction is the next step for a failing model, in increasing severity.
type RecoveryAction int

const (
	// RecoverRetry tries the same launch again.
	RecoverRetry RecoveryAction = iota
	// RecoverDegrade relaunches with a smaller footprint (shorter context, lower memory
	// use).
	RecoverDegrade
	// RecoverFallback serves a smaller model in this one's place, so something usable
	// stays available.
	RecoverFallback
	// RecoverQuarantine stops launching the model until its state is reset, so a model
	// that always fails cannot hold the loop in a restart cycle.
	RecoverQuarantine
)

// RecoveryState is a model's recent failure memory: the kind of the latest failure and
// the count of consecutive failed attempts, including that one. A successful launch
// resets it to the zero value.
type RecoveryState struct {
	Kind     FailureKind
	Attempts int
}

// Recover decides the next action for a failing model. Out of memory degrades, falls
// back, then quarantines, and is never retried unchanged. A hang gets one retry before
// degrading and falling back. A crash is retried a bounded number of times, then
// quarantined. Severity never decreases as attempts grow, so a persistently failing
// model always reaches quarantine.
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
