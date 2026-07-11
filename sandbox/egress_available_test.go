package sandbox

import (
	"runtime"
	"testing"
)

// TestGovernedEgressAvailableMatchesPlatform pins that the exported availability check
// reports the platforms with an enforcement leg (Linux, including WSL2, and macOS) as
// available and others (native Windows) as not, so a caller can refuse an external-agent
// episode with an actionable message where it cannot be governed.
func TestGovernedEgressAvailableMatchesPlatform(t *testing.T) {
	got := GovernedEgressAvailable()
	want := runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	if got != want {
		t.Errorf("GovernedEgressAvailable() = %v, want %v on %s", got, want, runtime.GOOS)
	}
}
