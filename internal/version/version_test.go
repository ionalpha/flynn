package version

import "testing"

// TestIsDevOnUnstampedBuild confirms an unstamped build (the test binary, with no release
// version linked in) reports itself as a development build, which is what routes it to a
// separate data directory.
func TestIsDevOnUnstampedBuild(t *testing.T) {
	if Version != devVersion {
		t.Skipf("build was stamped to %q; IsDev semantics only apply to an unstamped build", Version)
	}
	if !IsDev() {
		t.Fatal("IsDev() = false for an unstamped build, want true")
	}
}
