package text

import (
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// TestPropClipRuneSafe is the reason the package exists: over arbitrary strings
// (multibyte included) and limits, a clip is always valid UTF-8, never exceeds
// the limit plus the ellipsis, and returns short input untouched. The byte-slice
// version this replaces violated the first property.
func TestPropClipRuneSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		n := rapid.IntRange(0, 40).Draw(t, "n")
		got := Clip(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Clip(%q, %d) = %q is not valid UTF-8", s, n, got)
		}
		runes := len([]rune(s))
		if runes <= n {
			if got != s {
				t.Fatalf("short input must pass through: %q -> %q", s, got)
			}
		} else {
			if outRunes := len([]rune(got)); outRunes > n+3 {
				t.Fatalf("Clip(%q, %d) = %q has %d runes, cap is n+3", s, n, got, outRunes)
			}
		}
	})
}
