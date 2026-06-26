package main

import (
	"strings"
	"testing"
)

func TestRuntimeInstallUnknownRuntime(t *testing.T) {
	var b strings.Builder
	err := runRuntimeInstall([]string{"not-a-runtime"}, t.TempDir(), &b)
	if err == nil {
		t.Fatal("installing an unknown runtime should fail")
	}
	// The error names what Flynn can actually provision, so the user is not left guessing.
	if !strings.Contains(err.Error(), "llama.cpp") {
		t.Fatalf("error should name an installable runtime, got: %v", err)
	}
}

func TestInstallableRuntimes(t *testing.T) {
	names := installableRuntimes()
	if len(names) == 0 {
		t.Fatal("expected at least one installable runtime")
	}
	var hasLlama bool
	for _, n := range names {
		if n == "llama.cpp" {
			hasLlama = true
		}
	}
	if !hasLlama {
		t.Fatalf("llama.cpp should be installable, got %v", names)
	}
}
