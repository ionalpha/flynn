package pathglob

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// A walk prunes with MayContain, so it must never refuse a directory above a
// path that Match accepts.
func TestMayContainNeverPrunesAMatch(t *testing.T) {
	segment := rapid.SampledFrom([]string{"a", "b", "ab", "x.go", ".h"})
	patSegment := rapid.SampledFrom([]string{"a", "b", "*", "**", "?", "*.go", "{a,b}", "[ab]", ".h"})
	rapid.Check(t, func(t *rapid.T) {
		pattern := strings.Join(rapid.SliceOfN(patSegment, 1, 5).Draw(t, "pattern"), "/")
		name := rapid.SliceOfN(segment, 1, 5).Draw(t, "name")
		p, err := Compile(pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", pattern, err)
		}
		if !p.Match(strings.Join(name, "/")) {
			return
		}
		for i := 1; i < len(name); i++ {
			if dir := strings.Join(name[:i], "/"); !p.MayContain(dir) {
				t.Fatalf("%q matches %q but MayContain(%q) is false", pattern, strings.Join(name, "/"), dir)
			}
		}
	})
}
