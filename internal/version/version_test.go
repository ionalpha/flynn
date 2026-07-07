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

// TestStampedVersionWins confirms a release version linked in through ldflags is reported
// verbatim and is not treated as a development build.
func TestStampedVersionWins(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "v9.9.9"
	if got := String(); got != "v9.9.9" {
		t.Fatalf("String() = %q, want v9.9.9", got)
	}
	if IsDev() {
		t.Fatal("IsDev() = true for a stamped build, want false")
	}
}

// TestModuleVersionFallback confirms the `go install <module>@<version>` path: when no
// release version is stamped through ldflags, a real module version from the build info
// is used, while an absent version or Go's "(devel)" placeholder is not.
func TestModuleVersionFallback(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.1":         "v0.1.1",
		"v1.2.3-rc1":     "v1.2.3-rc1",
		"(devel)":        "",
		"":               "",
	} {
		if got := moduleVersion(in); got != want {
			t.Errorf("moduleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
