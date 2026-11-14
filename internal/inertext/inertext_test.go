package inertext_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/inertext"
)

func TestEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "shell", "shell"},
		{"path with backslash", `C:\work\tree`, `C:\work\tree`},
		{"tab and newline kept", "a\tb\nc", "a\tb\nc"},
		{"ansi escape", "rm\x1b[2K\x1b[1Gok", `rm\x1b[2K\x1b[1Gok`},
		{"carriage return", "safe\rEVIL", `safe\x0dEVIL`},
		{"nul and bell", "a\x00b\x07", `a\x00b\x07`},
		{"bidi override", "txt\u202egpj.exe", `txt\u202egpj.exe`},
		{"zero width space", "ad\u200bmin", `ad\u200bmin`},
		{"byte order mark", "\ufeffhost", `\ufeffhost`},
		{"line separator", "a\u2028b", `a\u2028b`},
		{"c1 control", "a\u009bb", `a\x9bb`},
		{"invalid utf-8", "a\xffb", `a\xffb`},
		{"non-ascii text kept", "résumé 日本語", "résumé 日本語"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inertext.Escape(tc.in); got != tc.want {
				t.Fatalf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapeOutputIsInert states the guarantee: nothing a terminal would act on survives,
// the result is valid UTF-8, and escaping a second time changes nothing.
func TestEscapeOutputIsInert(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := rapid.String().Draw(t, "in")
		out := inertext.Escape(in)
		if !utf8.ValidString(out) {
			t.Fatalf("output is not valid UTF-8: %q", out)
		}
		for _, r := range out {
			if r != '\t' && r != '\n' && (r < 0x20 || (r >= 0x7f && r < 0xa0) || r == 0x202e || r == 0x200b || r == 0xfeff) {
				t.Fatalf("output %q still holds %U", out, r)
			}
		}
		if again := inertext.Escape(out); again != out {
			t.Fatalf("not idempotent: %q then %q", out, again)
		}
	})
}

func TestLimit(t *testing.T) {
	if got := inertext.Limit("abcdef", 3); got != "abc" {
		t.Fatalf("Limit = %q", got)
	}
	if got := inertext.Limit("abc", 10); got != "abc" {
		t.Fatalf("a short string was cut: %q", got)
	}
	if got := inertext.Limit("abc", 0); got != "" {
		t.Fatalf("a zero limit kept %q", got)
	}
	// A three-byte character straddling the limit is dropped whole, not split.
	got := inertext.Limit("a日本", 3)
	if got != "a" || !utf8.ValidString(got) {
		t.Fatalf("Limit split a character: %q", got)
	}
	if long := inertext.Limit(strings.Repeat("é", 100), 7); !utf8.ValidString(long) || len(long) > 7 {
		t.Fatalf("Limit = %q", long)
	}
}
