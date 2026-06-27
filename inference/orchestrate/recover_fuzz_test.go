package orchestrate

import "testing"

// FuzzRecover throws arbitrary (possibly out-of-range or negative) failure kinds and attempt
// counts at the policy and asserts it never panics and always returns a defined action. An
// unrecognized kind must degrade to the safe default rather than an undefined result, so a
// malformed failure record can never produce an undefined recovery.
func FuzzRecover(f *testing.F) {
	f.Add(0, 0)
	f.Add(2, 1)   // OOM, first attempt
	f.Add(3, 3)   // hang, third attempt
	f.Add(-1, -5) // nonsense
	f.Add(99, 1000)

	f.Fuzz(func(t *testing.T, kind, attempts int) {
		got := Recover(RecoveryState{Kind: FailureKind(kind), Attempts: attempts})
		switch got {
		case RecoverRetry, RecoverDegrade, RecoverFallback, RecoverQuarantine:
		default:
			t.Fatalf("Recover returned an undefined action %d for kind=%d attempts=%d", got, kind, attempts)
		}
	})
}
