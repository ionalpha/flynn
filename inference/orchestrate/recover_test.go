package orchestrate

import "testing"

func TestRecoverOOMNeverRetriesUnchanged(t *testing.T) {
	// An identical retry cannot fix an out-of-memory failure, so the first OOM degrades and
	// it escalates from there; RecoverRetry must never be the answer to an OOM.
	for attempts := 1; attempts <= 6; attempts++ {
		got := Recover(RecoveryState{Kind: FailureOOM, Attempts: attempts})
		if got == RecoverRetry {
			t.Fatalf("OOM at attempt %d returned RecoverRetry, which is futile", attempts)
		}
	}
	if got := Recover(RecoveryState{Kind: FailureOOM, Attempts: 1}); got != RecoverDegrade {
		t.Fatalf("first OOM = %v, want RecoverDegrade", got)
	}
	if got := Recover(RecoveryState{Kind: FailureOOM, Attempts: 2}); got != RecoverFallback {
		t.Fatalf("second OOM = %v, want RecoverFallback", got)
	}
	if got := Recover(RecoveryState{Kind: FailureOOM, Attempts: 3}); got != RecoverQuarantine {
		t.Fatalf("third OOM = %v, want RecoverQuarantine", got)
	}
}

func TestRecoverHangRetriesOnceThenEscalates(t *testing.T) {
	steps := []RecoveryAction{RecoverRetry, RecoverDegrade, RecoverFallback, RecoverQuarantine}
	for i, want := range steps {
		if got := Recover(RecoveryState{Kind: FailureHang, Attempts: i + 1}); got != want {
			t.Fatalf("hang at attempt %d = %v, want %v", i+1, got, want)
		}
	}
}

func TestRecoverCrashRetriesThenQuarantines(t *testing.T) {
	if got := Recover(RecoveryState{Kind: FailureCrash, Attempts: 1}); got != RecoverRetry {
		t.Fatalf("first crash = %v, want RecoverRetry", got)
	}
	if got := Recover(RecoveryState{Kind: FailureCrash, Attempts: 2}); got != RecoverRetry {
		t.Fatalf("second crash = %v, want RecoverRetry", got)
	}
	if got := Recover(RecoveryState{Kind: FailureCrash, Attempts: 3}); got != RecoverQuarantine {
		t.Fatalf("third crash = %v, want RecoverQuarantine (crash loop)", got)
	}
}

func TestRecoverZeroAttemptsTreatedAsFirst(t *testing.T) {
	// A state with no recorded attempts is treated as the first failure, never panicking or
	// escalating early.
	if got := Recover(RecoveryState{Kind: FailureOOM, Attempts: 0}); got != RecoverDegrade {
		t.Fatalf("OOM with zero attempts = %v, want the first-attempt action RecoverDegrade", got)
	}
}
