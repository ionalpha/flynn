package dependency

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestVersionDirContainmentProperty asserts the invariant the install path relies on: for
// any pinned version string, the directory name it maps to is a single, safe path element.
// It never empties, never contains a path separator or a drive colon, so distinct versions
// always install into distinct sibling directories under the dependency's install root and
// a crafted version string cannot redirect the install elsewhere.
func TestVersionDirContainmentProperty(t *testing.T) {
	rune8 := rapid.SampledFrom([]rune{'0', '1', '9', '.', '/', '\\', ':', ' ', 'v', '-', 'a', 'β'})
	rapid.Check(t, func(rt *rapid.T) {
		pin := string(rapid.SliceOfN(rune8, 0, 20).Draw(rt, "pin"))
		dir := versionDir(pin)
		if dir == "" {
			rt.Fatalf("versionDir(%q) is empty", pin)
		}
		if strings.ContainsAny(dir, `/\:`) {
			rt.Fatalf("versionDir(%q)=%q is not a single path element", pin, dir)
		}
		if strings.TrimSpace(dir) != dir {
			rt.Fatalf("versionDir(%q)=%q has surrounding space", pin, dir)
		}
	})
}
