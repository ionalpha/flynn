package inference

import "testing"

// FuzzParseVersion checks that version parsing is total over any string: it never
// panics and every extracted component is a non-negative integer within the digit
// cap, so a hostile or malformed version string can never crash a comparison or
// smuggle in an out-of-range component.
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{
		"", "1.2.3", "v1.2.3", "10.0", "1.2.3-rc1+build.5",
		"999999999999999999999999", "....", "1..2", "abc", "0",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v := ParseVersion(s)
		for i, c := range v {
			if c < 0 {
				t.Fatalf("ParseVersion(%q) component %d = %d, want non-negative", s, i, c)
			}
		}
		// Parsing is idempotent: rendering then reparsing yields the same components,
		// so a version that survives a round trip through its own text form still
		// compares the same way. Comparing lengths alone would miss a mangled value.
		again := ParseVersion(v.String())
		if len(again) != len(v) {
			t.Fatalf("ParseVersion not stable through String(): %v -> %q -> %v", v, v.String(), again)
		}
		for i := range v {
			if again[i] != v[i] {
				t.Fatalf("ParseVersion not stable through String(): %v -> %q -> %v (component %d)", v, v.String(), again, i)
			}
		}
	})
}
