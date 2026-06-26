package inference

import (
	"strings"
	"testing"
)

func TestParseVersionFromRuntimeOutput(t *testing.T) {
	cases := []struct {
		name string
		rt   Runtime
		raw  string
		want string
	}{
		{"ollama", Ollama, "ollama version is 0.3.14", "0.3.14"},
		{"ollama two-part", Ollama, "ollama version is 0.12", "0.12"},
		{"llama.cpp version line", LlamaCpp, "version: 5662 (a1b2c3d)\nbuilt with cc 13.2", "5662"},
		{"llama.cpp b-prefixed", LlamaCpp, "version: b8146 (deadbee)", "8146"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rt.ParseVersion(tc.raw).String(); got != tc.want {
				t.Fatalf("ParseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"0.3.13", "0.3.14", true},
		{"0.3.14", "0.3.14", false},
		{"0.3", "0.3.1", true},
		{"5661", "5662", true},
		{"8146", "8145", false},
		{"1.0.0", "0.9.9", false},
	}
	for _, tc := range cases {
		got := ParseVersion(tc.a).Less(ParseVersion(tc.b))
		if got != tc.less {
			t.Errorf("Less(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.less)
		}
	}
}

// TestSafeToRunGatesKnownCVEs exercises the gate against the real built-in advisories:
// a llama.cpp build below the fix is refused and the error names the CVE, while a build
// at or past every fix, and a runtime with no known advisory, are allowed.
func TestSafeToRunGatesKnownCVEs(t *testing.T) {
	// Below the GGUF-parser fix (b8146): refused, error names the CVE.
	if err := SafeToRun("llama.cpp", ParseVersion("5662")); err == nil {
		t.Fatal("a llama.cpp build below the parser fix must be refused")
	} else if !strings.Contains(err.Error(), "CVE-2026-27940") {
		t.Fatalf("the refusal must name the advisory, got %v", err)
	}

	// Below the vocabulary-loader fix (b5662): refused for that CVE too.
	if err := SafeToRun("llama.cpp", ParseVersion("5000")); err == nil {
		t.Fatal("an old llama.cpp build must be refused")
	} else if !strings.Contains(err.Error(), "CVE-2025-49847") {
		t.Fatalf("the refusal must name the vocabulary-loader advisory, got %v", err)
	}

	// At or past every fix: allowed.
	if err := SafeToRun("llama.cpp", ParseVersion("8146")); err != nil {
		t.Fatalf("a patched llama.cpp build must be allowed, got %v", err)
	}
	if err := SafeToRun("llama.cpp", ParseVersion("9000")); err != nil {
		t.Fatalf("a newer llama.cpp build must be allowed, got %v", err)
	}

	// A runtime with no advisory in the list is not gated by version here.
	if err := SafeToRun("ollama", ParseVersion("0.1.0")); err != nil {
		t.Fatalf("a runtime with no known advisory must not be refused, got %v", err)
	}
}
