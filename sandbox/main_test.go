package sandbox

import (
	"os"
	"testing"
)

// TestMain lets the test binary stand in for the real program when a filesystem-
// confinement test re-executes it as a confinement launcher. Without this, the
// re-executed test binary would rerun the whole test suite inside the namespaces
// instead of building the mount view and execing the command. In the ordinary case
// RunChildLaunchIfRequested returns immediately and the tests run as normal.
func TestMain(m *testing.M) {
	RunChildLaunchIfRequested()
	os.Exit(m.Run())
}
