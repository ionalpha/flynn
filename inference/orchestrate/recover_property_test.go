package orchestrate

import (
	"testing"

	"pgregory.net/rapid"
)

// TestRecoverEscalatesMonotonically asserts the recovery ladder only ever climbs: for any
// failure kind, the action at one more attempt is at least as severe as at the current
// attempt. This is what guarantees the loop tightens toward quarantine instead of oscillating.
func TestRecoverEscalatesMonotonically(t *testing.T) {
	kinds := []FailureKind{FailureNone, FailureCrash, FailureOOM, FailureHang}
	rapid.Check(t, func(rt *rapid.T) {
		kind := rapid.SampledFrom(kinds).Draw(rt, "kind")
		attempts := rapid.IntRange(1, 50).Draw(rt, "attempts")
		now := Recover(RecoveryState{Kind: kind, Attempts: attempts})
		next := Recover(RecoveryState{Kind: kind, Attempts: attempts + 1})
		if next < now {
			rt.Fatalf("recovery de-escalated for %v: attempt %d -> %v, attempt %d -> %v",
				kind, attempts, now, attempts+1, next)
		}
	})
}

// TestRecoverFailuresReachQuarantine asserts that any real failure kind, given enough
// consecutive attempts, ends in quarantine, so a persistently failing model can never be
// retried forever.
func TestRecoverFailuresReachQuarantine(t *testing.T) {
	for _, kind := range []FailureKind{FailureCrash, FailureOOM, FailureHang} {
		if got := Recover(RecoveryState{Kind: kind, Attempts: 100}); got != RecoverQuarantine {
			t.Fatalf("%v never quarantines: at 100 attempts got %v", kind, got)
		}
	}
}

// TestRecoverOOMNeverRetries restates the key safety property over the whole attempt range:
// an out-of-memory failure is never answered with an identical retry.
func TestRecoverOOMNeverRetries(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		attempts := rapid.IntRange(0, 1000).Draw(rt, "attempts")
		if Recover(RecoveryState{Kind: FailureOOM, Attempts: attempts}) == RecoverRetry {
			rt.Fatalf("OOM at attempt %d returned RecoverRetry", attempts)
		}
	})
}
