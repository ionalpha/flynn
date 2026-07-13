package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// A version this package cannot parse is never compared, because a comparison that
// quietly treats an unparseable version as zero is a downgrade attack with extra steps.
// These are the spellings that must not parse.
func TestParseVersionIsStrict(t *testing.T) {
	rejected := []string{
		"",
		"latest",
		"v1.2",
		"v1.2.3.4",
		"v1.2.x",
		"v-1.2.3",
		"v01.2.3",             // a leading zero makes "01" and "1" two spellings of one version
		"v1.02.3",             //
		"v1.2.3+build.5",      // build metadata does not participate in precedence
		"v1.2.3-rc.1+build",   //
		"v1.2.3-",             // a prerelease marker with no prerelease
		"v1.2.3-rc..1",        // an empty prerelease identifier
		"v1.2.3-rc_1",         // an identifier outside the alphabet the specification allows
		"v1.2.3-rc/../../etc", //
	}
	for _, s := range rejected {
		if v, ok := ParseVersion(s); ok {
			t.Errorf("ParseVersion(%q) accepted it as %s", s, v)
		}
	}

	accepted := map[string]bool{ // version -> is a prerelease
		"v1.2.3":        false,
		"1.2.3":         false, // the stamped version carries no leading "v"
		"v0.0.0":        false,
		"v1.2.3-rc.1":   true,
		"v1.2.3-alpha":  true,
		"v1.2.3-0":      true,
		"v1.2.3-rc.1.1": true,
		"v10.20.30-x-y": true,
	}
	for s, pre := range accepted {
		v, ok := ParseVersion(s)
		if !ok {
			t.Errorf("ParseVersion(%q) refused a version flynn publishes", s)
			continue
		}
		if v.String() != s {
			t.Errorf("ParseVersion(%q).String() = %q", s, v.String())
		}
		if v.IsPrerelease() != pre {
			t.Errorf("%q: prerelease = %v, want %v", s, v.IsPrerelease(), pre)
		}
	}
}

// Precedence decides which way an upgrade goes. The rules that matter here are the ones
// an attacker would want wrong: a prerelease must sort below its release, and numeric
// prerelease identifiers must compare as numbers, so rc.10 is not "older" than rc.9.
func TestVersionPrecedence(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.3", "1.2.3", 0}, // the tag and the stamped version are one version
		{"v2.0.0", "v1.9.9", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.2.3", "v1.2.3-rc.1", 1},        // a release outranks any prerelease of itself
		{"v1.2.3-rc.1", "v1.2.3", -1},       //
		{"v1.2.3-rc.10", "v1.2.3-rc.9", 1},  // numeric identifiers compare as numbers
		{"v1.2.3-rc.2", "v1.2.3-rc.10", -1}, //
		{"v1.2.3-alpha", "v1.2.3-beta", -1}, // and alphanumeric ones as text
		{"v1.2.3-1", "v1.2.3-alpha", -1},    // a numeric identifier sorts below an alphanumeric one
		{"v1.2.3-alpha", "v1.2.3-1", 1},     //
		{"v1.2.3-rc.1.1", "v1.2.3-rc.1", 1}, // a longer prerelease outranks the prefix it extends
		{"v1.2.3-rc.1", "v1.2.3-rc.1", 0},
	}
	for _, tc := range tests {
		a, b := mustVersion(t, tc.a), mustVersion(t, tc.b)
		if got := a.Compare(b); got != tc.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// The upgrade memory is what defeats the two attacks no signature can catch: a rollback
// to a genuinely signed old release, and a freeze that shows nothing new. It has to
// survive a restart, and it has to fail safe when it cannot be read.
func TestStateRoundTripsAndDegradesToNoMemory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	// Nothing written yet: no memory, and no error. A machine that refused to upgrade
	// because it could not read its own notes is a machine that stays unpatched.
	if st := loadState(dir); st != (state{}) {
		t.Fatalf("an unwritten state is not empty: %+v", st)
	}

	want := state{HighestVerified: "v0.5.0", NewestSeen: "v0.6.0"}
	if err := saveState(dir, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got := loadState(dir)
	if got.HighestVerified != want.HighestVerified || got.NewestSeen != want.NewestSeen {
		t.Fatalf("state = %+v, want %+v", got, want)
	}

	// A state file that has been corrupted is read as no memory rather than as a version,
	// because a half-parsed record of what was verified is worse than none.
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := loadState(dir); st != (state{}) {
		t.Fatalf("a corrupt state file was read as %+v", st)
	}
	// And the floor degrades to the running version, which is still a floor.
	if floor := loadState(dir).floor(mustVersion(t, "v0.3.0")); floor.String() != "v0.3.0" {
		t.Fatalf("floor = %s, want the running version", floor)
	}
}

// The state cannot be written where a file already sits, and it says so rather than
// carrying on as if the memory had been kept.
func TestSaveStateRefusesADataDirItCannotCreate(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := saveState(filepath.Join(blocked, "data"), state{HighestVerified: "v0.5.0"})
	if err == nil {
		t.Fatal("the state was written under a path that is a file")
	}
	if codeOf(t, err) != CodeState {
		t.Fatalf("code = %q, want %q", codeOf(t, err), CodeState)
	}
}

// The freeze check only fires when the listing has actually gone backwards. A listing
// offering the same release, or a newer one, is not evidence of anything.
func TestStaleListingOnlyFiresOnAListingThatWentBackwards(t *testing.T) {
	tests := []struct {
		seen, newest string
		wantStale    bool
	}{
		{"v0.4.0", "v0.3.0", true},
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.5.0", false},
		{"", "v0.5.0", false},              // nothing has been seen yet
		{"not a version", "v0.5.0", false}, // and an unparseable memory is no memory
	}
	for _, tc := range tests {
		prev, stale := state{NewestSeen: tc.seen}.staleListing(mustVersion(t, tc.newest))
		if stale != tc.wantStale {
			t.Errorf("staleListing(seen=%q, newest=%q) = %q, %v; want stale=%v", tc.seen, tc.newest, prev, stale, tc.wantStale)
		}
		if stale && prev != tc.seen {
			t.Errorf("staleListing named %q, want %q", prev, tc.seen)
		}
	}
}

// isSourceBuild has to catch every shape a build that no release published can take,
// because a build it misses is one `flynn upgrade` would quietly throw away.
func TestSourceBuildsAreRecognised(t *testing.T) {
	for _, v := range []string{"", "0.0.0-dev", "0.0.0-dev.1", "v1.2.3+abc123"} {
		if !isSourceBuild(v) {
			t.Errorf("isSourceBuild(%q) = false", v)
		}
	}
	for _, v := range []string{"v1.2.3", "1.2.3", "v0.1.3-rc.1"} {
		if isSourceBuild(v) {
			t.Errorf("isSourceBuild(%q) = true, so a released build cannot upgrade", v)
		}
	}
}
