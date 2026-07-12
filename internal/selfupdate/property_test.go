package selfupdate

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// versionGen draws a version tag of the shape flynn publishes.
func versionGen(t *rapid.T, label string) Version {
	core := fmt.Sprintf("v%d.%d.%d",
		rapid.IntRange(0, 20).Draw(t, label+".major"),
		rapid.IntRange(0, 20).Draw(t, label+".minor"),
		rapid.IntRange(0, 20).Draw(t, label+".patch"))
	if rapid.Bool().Draw(t, label+".prerelease") {
		core += "-" + rapid.SampledFrom([]string{
			"rc.1", "rc.2", "rc.10", "alpha", "beta", "beta.2", "0", "1", "1.1",
		}).Draw(t, label+".pre")
	}
	v, ok := ParseVersion(core)
	if !ok {
		t.Fatalf("the generator produced an unparseable version: %s", core)
	}
	return v
}

// The version comparison decides which way an upgrade goes, so it has to be a total
// order. A comparison that is not antisymmetric, or not transitive, is a comparison an
// attacker can stand between: it means there exist versions a and b where a is both
// newer and older than b, and the downgrade check can be walked around.
func TestVersionComparisonIsATotalOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := versionGen(t, "a")
		b := versionGen(t, "b")
		c := versionGen(t, "c")

		if got, want := a.Compare(b), -b.Compare(a); got != want {
			t.Fatalf("Compare(%s,%s)=%d but Compare(%s,%s)=%d", a, b, got, b, a, b.Compare(a))
		}
		if a.Compare(a) != 0 {
			t.Fatalf("%s does not equal itself", a)
		}
		if a.Compare(b) <= 0 && b.Compare(c) <= 0 && a.Compare(c) > 0 {
			t.Fatalf("not transitive: %s <= %s <= %s but %s > %s", a, b, c, a, c)
		}
	})
}

// A prerelease is always older than the release it leads to. This is the rule that
// keeps `flynn upgrade` from walking a machine from v0.2.0 back onto v0.2.0-rc.1,
// which is a downgrade wearing a newer-looking name.
func TestAPrereleaseIsOlderThanItsRelease(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v := versionGen(t, "v")
		if !v.IsPrerelease() {
			return
		}
		core, _, _ := strings.Cut(v.String(), "-")
		released, ok := ParseVersion(core)
		if !ok {
			t.Fatalf("%q does not parse", core)
		}
		if v.Compare(released) >= 0 {
			t.Fatalf("prerelease %s does not sort before %s", v, released)
		}
	})
}

// The floor never falls. Whatever the state says and whatever is running, the version
// an upgrade must not go below is at least the version that is running: a machine
// cannot be talked into lowering its own bar.
func TestTheDowngradeFloorNeverFallsBelowWhatIsRunning(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		current := versionGen(t, "current")
		remembered := rapid.SampledFrom([]string{
			"", "not a version", "v0.0.1", "v99.0.0", "v0.1.3-rc.1",
		}).Draw(t, "remembered")

		floor := state{HighestVerified: remembered}.floor(current)
		if floor.Compare(current) < 0 {
			t.Fatalf("floor %s is below the running version %s", floor, current)
		}
		if v, ok := ParseVersion(remembered); ok && floor.Compare(v) < 0 {
			t.Fatalf("floor %s is below the highest version ever verified (%s)", floor, v)
		}
	})
}

// The extractor takes one file out of an archive and it is always the one it went
// looking for. No entry name, however it is spelled, may be matched as the binary
// unless it is exactly the binary: an archive that can name a path is an archive that
// can name a path outside the destination.
func TestNoEscapingArchiveEntryIsEverMatched(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		binary := rapid.SampledFrom([]string{"flynn", "flynn.exe"}).Draw(t, "binary")
		entry := rapid.SampledFrom([]string{
			"../flynn", "../../flynn", "/flynn", "//flynn", `..\flynn`, `C:\flynn.exe`,
			"./../flynn", "dir/../../flynn", "flynn/../../../flynn", `\\host\share\flynn.exe`,
			"~/flynn", "sub/flynn", "flynn.exe.exe", "FLYNN", " flynn",
		}).Draw(t, "entry")

		if matchesEntry(entry, binary) {
			t.Fatalf("the extractor would have taken %q as %q", entry, binary)
		}
	})
	// The one name it does match is the plain one, with or without a leading "./".
	for _, binary := range []string{"flynn", "flynn.exe"} {
		if !matchesEntry(binary, binary) || !matchesEntry("./"+binary, binary) {
			t.Fatalf("the extractor does not recognise its own binary %q", binary)
		}
	}
}

// The asset a machine downloads is named from its own platform, never from anything a
// server said, and it is always a plain file name.
func TestTheAssetNameIsAlwaysAPlainFileName(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := &Updater{
			goos:   rapid.SampledFrom([]string{"linux", "darwin", "windows"}).Draw(t, "goos"),
			goarch: rapid.SampledFrom([]string{"amd64", "arm64"}).Draw(t, "goarch"),
		}
		name := u.assetName()
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Fatalf("asset name %q is not a plain file name", name)
		}
		if !strings.HasPrefix(name, "flynn_") {
			t.Fatalf("asset name %q is not a flynn archive", name)
		}
		if (u.goos == "windows") != strings.HasSuffix(name, ".zip") {
			t.Fatalf("asset name %q does not match the archive format for %s", name, u.goos)
		}
	})
}
