package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/catalog"
)

func TestRunModelFetchBranches(t *testing.T) {
	dir := t.TempDir()

	// A catalog entry with no pinned URL is reported as not-downloadable, not fetched.
	var unpinned strings.Builder
	if err := runModelFetch([]string{"ollama:qwen2.5-coder:1.5b"}, dir, &unpinned); err != nil {
		t.Fatalf("unpinned entry should not error: %v", err)
	}
	if !strings.Contains(unpinned.String(), "no pinned download URL") {
		t.Fatalf("expected the not-downloadable message, got %q", unpinned.String())
	}

	// A hosted API model has nothing to download.
	if err := runModelFetch([]string{"anthropic:claude-opus-4-8"}, dir, &strings.Builder{}); err == nil {
		t.Fatal("fetching an API model should error")
	}
	// An unknown id errors.
	if err := runModelFetch([]string{"nope:nope"}, dir, &strings.Builder{}); err == nil {
		t.Fatal("unknown id should error")
	}
	// No id errors.
	if err := runModelFetch(nil, dir, &strings.Builder{}); err == nil {
		t.Fatal("missing id should error")
	}
}

func TestWeightsFileNameIsSafe(t *testing.T) {
	name := weightsFileName("ollama:qwen2.5-coder:1.5b", catalog.Quant{Name: "Q4_K_M"})
	if strings.ContainsAny(name, ":/\\ ") {
		t.Fatalf("file name not filesystem-safe: %q", name)
	}
	if !strings.HasSuffix(name, ".gguf") {
		t.Fatalf("want a .gguf name, got %q", name)
	}
}

func TestSizeCeiling(t *testing.T) {
	if sizeCeiling(0) != 0 {
		t.Fatal("unknown size keeps the default ceiling (0)")
	}
	if got := sizeCeiling(1000); got != 1050 {
		t.Fatalf("sizeCeiling(1000)=%d, want 1050 (+5%%)", got)
	}
}
