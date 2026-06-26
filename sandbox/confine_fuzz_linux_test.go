//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

// FuzzArgvRoundTrip proves the command survives the trip across the re-exec intact:
// whatever bytes a command line holds (newlines, commas, quotes, non-UTF-8, the empty
// string), encoding it and decoding it back yields exactly the same arguments. A
// command that did not round-trip would run mangled or not at all.
func FuzzArgvRoundTrip(f *testing.F) {
	for _, seed := range []string{"", "echo hi", "a,b,c", "x\ny", `"q"`, "\x00\xff", strings.Repeat("z", 4096)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		argv := []string{"sh", "-c", line}
		got, err := decodeArgv(encodeArgv(argv))
		if err != nil {
			t.Fatalf("round-trip of %q failed to decode: %v", line, err)
		}
		if len(got) != len(argv) {
			t.Fatalf("round-trip changed argument count: got %d, want %d", len(got), len(argv))
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Fatalf("round-trip changed argument %d: got %q, want %q", i, got[i], argv[i])
			}
		}
	})
}

// FuzzDecodeArgv proves the decoder is robust to arbitrary, possibly malformed input
// from the control variable: it must return an error or a result, never panic. The
// launcher treats any decode error as a refusal to run, so robustness here is what
// keeps a corrupted control variable from crashing the launcher.
func FuzzDecodeArgv(f *testing.F) {
	for _, seed := range []string{"", ",", "!!!", "c2g=", "c2g=,LWM=", "not base64 at all"} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, s string) {
		_, _ = decodeArgv(s) // must not panic on any input
	})
}

// FuzzUnescapeOctal proves the mountinfo path decoder is total: for any input, it
// returns without panicking and never lengthens the string (each escape it resolves
// shortens it, and everything else is copied through), and a string with no backslash
// is returned unchanged. Malformed or truncated escapes must be passed through, not
// crash the launcher that is reading the mount table.
func FuzzUnescapeOctal(f *testing.F) {
	for _, seed := range []string{"", `\040`, `\1`, `\\`, `\777`, `\999`, `/a b/c`, `trailing\`, `\04`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := unescapeOctal(s)
		if len(out) > len(s) {
			t.Fatalf("unescape lengthened %q to %q", s, out)
		}
		if !strings.Contains(s, `\`) && out != s {
			t.Fatalf("unescape changed a backslash-free string %q to %q", s, out)
		}
	})
}
