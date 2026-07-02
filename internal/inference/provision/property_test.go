package provision

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestPinnedReleaseInstallableProperty asserts the invariant the runtime provisioner owns:
// every release in the shipped registry is installable. For any pinned release, the
// version gate passes (so it is never refused at run time), every field the install path
// needs is present, and its versioned build directory composes under the destination root
// rather than escaping it. A registry entry that violates this would make provisioning a
// dead end on some platform, so the property guards the data the binary ships with.
func TestPinnedReleaseInstallableProperty(t *testing.T) {
	all := Releases()
	if len(all) == 0 {
		t.Fatal("expected pinned releases")
	}
	rapid.Check(t, func(rt *rapid.T) {
		rel := all[rapid.IntRange(0, len(all)-1).Draw(rt, "release")]

		if err := rel.Gate(); err != nil {
			rt.Fatalf("pinned release %s/%s %s fails the version gate: %v", rel.GOOS, rel.GOARCH, rel.Version, err)
		}
		if rel.URL == "" || rel.SHA256 == "" || rel.BinName == "" || rel.Runtime == "" {
			rt.Fatalf("pinned release %s/%s is missing a required field: %+v", rel.GOOS, rel.GOARCH, rel)
		}
		if !strings.HasPrefix(rel.URL, "https://") {
			rt.Fatalf("pinned release %s/%s is not fetched over https: %q", rel.GOOS, rel.GOARCH, rel.URL)
		}
		// The version directory must be a non-empty, single path element so a build
		// installs under destDir/runtime/<version> and versions never collide.
		v := rel.Version.String()
		if v == "" || strings.ContainsAny(v, `/\`) {
			rt.Fatalf("pinned release %s/%s has an unusable version dir name %q", rel.GOOS, rel.GOARCH, v)
		}
	})
}
