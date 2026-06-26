package provision

import (
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestSafeJoinContainmentProperty asserts the core security invariant of extraction:
// for any archive entry name, safeJoin either rejects it or returns a path that stays
// strictly within the install directory. No generated name, however it mixes
// separators, "..", absolute roots, or empty segments, may resolve outside base.
func TestSafeJoinContainmentProperty(t *testing.T) {
	base := filepath.Clean(t.TempDir())
	segment := rapid.SampledFrom([]string{"a", "b", "dir", "..", ".", "", "x.txt", "llama-server"})
	rapid.Check(t, func(rt *rapid.T) {
		sep := rapid.SampledFrom([]string{"/", `\`}).Draw(rt, "sep")
		parts := rapid.SliceOfN(segment, 1, 6).Draw(rt, "parts")
		name := strings.Join(parts, sep)
		if rapid.Bool().Draw(rt, "absolute") {
			name = sep + name
		}

		dst, ok := safeJoin(base, name)
		if !ok {
			return // a rejected entry is always acceptable
		}
		// An accepted entry must resolve under base with no upward escape.
		rel, err := filepath.Rel(base, dst)
		if err != nil {
			rt.Fatalf("accepted %q gives unrelatable path %q: %v", name, dst, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			rt.Fatalf("accepted %q escaped base: rel=%q dst=%q", name, rel, dst)
		}
		if !strings.HasPrefix(dst, base) {
			rt.Fatalf("accepted %q resolved outside base: %q", name, dst)
		}
	})
}
