package version

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// hexShort matches the shortened VCS revision String appends to a dev version.
var hexShort = regexp.MustCompile(`^[0-9a-f]{1,12}$`)

// TestRevisionShapeMatchesBuildInfo checks Revision reports exactly the vcs.revision the
// toolchain stamped, truncated to 12 characters, and reports nothing when the build carries
// no revision. Both outcomes are legitimate (a released archive has no VCS stamp), so the
// test asserts the relationship to the build info rather than a fixed value.
func TestRevisionShapeMatchesBuildInfo(t *testing.T) {
	got := Revision()

	var stamped string
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				stamped = s.Value
			}
		}
	}
	if stamped == "" {
		if got != "" {
			t.Fatalf("Revision() = %q with no vcs.revision in the build info", got)
		}
		return
	}
	want := stamped
	if len(want) > 12 {
		want = want[:12]
	}
	if got != want {
		t.Fatalf("Revision() = %q, want %q (vcs.revision %q shortened)", got, want, stamped)
	}
	if len(got) > 12 {
		t.Fatalf("Revision() = %q, longer than the 12-character short form", got)
	}
	if !hexShort.MatchString(got) {
		t.Fatalf("Revision() = %q, want a short hex revision", got)
	}
}

// TestStringOnDevBuild checks the unstamped-build shape: the source default, with the VCS
// revision appended when the toolchain stamped one. A stamped release build takes the other
// branch, covered by TestStampedVersionWins.
func TestStringOnDevBuild(t *testing.T) {
	if !IsDev() {
		t.Skipf("build reports release version %q; the dev-build shape does not apply", String())
	}
	got := String()
	rev := Revision()
	if rev == "" {
		if got != Version {
			t.Fatalf("String() = %q, want the bare source default %q with no VCS stamp", got, Version)
		}
		return
	}
	want := Version + "+" + rev
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, Version+"+") {
		t.Fatalf("String() = %q, want the source default plus a revision", got)
	}
}

// TestRevisionFromSettings covers the shortening rule over the build settings a binary can
// carry: a full commit hash is cut to the 12-character short form, a shorter one is passed
// through, and anything that is not a usable revision yields nothing.
func TestRevisionFromSettings(t *testing.T) {
	setting := func(k, v string) debug.BuildSetting { return debug.BuildSetting{Key: k, Value: v} }

	cases := []struct {
		name string
		in   []debug.BuildSetting
		want string
	}{
		{
			name: "full hash is shortened",
			in:   []debug.BuildSetting{setting("vcs", "git"), setting("vcs.revision", "0123456789abcdef0123456789abcdef01234567")},
			want: "0123456789ab",
		},
		{"exactly twelve", []debug.BuildSetting{setting("vcs.revision", "0123456789ab")}, "0123456789ab"},
		{"shorter than twelve", []debug.BuildSetting{setting("vcs.revision", "abc123")}, "abc123"},
		{"empty value", []debug.BuildSetting{setting("vcs.revision", "")}, ""},
		{"no vcs setting", []debug.BuildSetting{setting("vcs.modified", "true"), setting("-tags", "netgo")}, ""},
		{"no settings at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := revisionFrom(tc.in); got != tc.want {
				t.Fatalf("revisionFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStringAppendsRevisionToDevBuild checks the dev-build string carries the revision the
// binary was built from, which is what lets a captured diagnostic be read against its
// source. A build with no VCS stamp reports the bare source default instead.
func TestStringAppendsRevisionToDevBuild(t *testing.T) {
	oldVersion, oldRev := Version, buildRevision
	t.Cleanup(func() { Version, buildRevision = oldVersion, oldRev })
	Version = devVersion

	buildRevision = func() string { return "0123456789ab" }
	if got, want := String(), devVersion+"+0123456789ab"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	buildRevision = func() string { return "" }
	if got := String(); got != devVersion {
		t.Fatalf("String() = %q with no VCS stamp, want the bare source default %q", got, devVersion)
	}
}

// TestReleasedPrefersStampedOverModuleVersion checks the precedence in released(): an
// ldflags-stamped version wins outright, and it is reported verbatim by String() without a
// revision appended, since a release is identified by its tag and not by a commit.
func TestReleasedPrefersStampedOverModuleVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v1.4.2"
	if got := released(); got != "v1.4.2" {
		t.Fatalf("released() = %q, want the stamped v1.4.2", got)
	}
	if got := String(); got != "v1.4.2" {
		t.Fatalf("String() = %q, want v1.4.2 with no revision suffix", got)
	}
	if strings.Contains(String(), "+") {
		t.Fatalf("String() = %q, a release must not carry a revision suffix", String())
	}
	if IsDev() {
		t.Fatal("IsDev() = true for a stamped release build")
	}

	// Back to the source default: the build is a dev build again, so the module-version
	// fallback (absent in a test binary) leaves it unversioned.
	Version = devVersion
	if !IsDev() {
		t.Fatal("IsDev() = false after restoring the unstamped source default")
	}
}
