package main

import (
	"strings"
	"testing"
)

func TestRunModelsLists(t *testing.T) {
	var b strings.Builder
	if err := runModels(nil, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"MODEL", "KIND", "anthropic:claude-opus-4-8", "ollama:qwen2.5-coder:1.5b", "blessed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("models output missing %q:\n%s", want, out)
		}
	}
}

func TestRunModelsFilters(t *testing.T) {
	// --local drops API models.
	var local strings.Builder
	if err := runModels([]string{"--local"}, &local); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(local.String(), "anthropic:claude-opus-4-8") {
		t.Fatalf("--local should drop API models:\n%s", local.String())
	}
	if !strings.Contains(local.String(), "ollama:") {
		t.Fatal("--local should keep local models")
	}

	// --max-size keeps only the small local model.
	var small strings.Builder
	if err := runModels([]string{"--local", "--max-size", "2"}, &small); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small.String(), "qwen2.5-coder:7b") {
		t.Fatalf("--max-size 2 should drop the 7B model:\n%s", small.String())
	}
	if !strings.Contains(small.String(), "qwen2.5-coder:1.5b") {
		t.Fatal("--max-size 2 should keep the 1.5B model")
	}

	// A filter that matches nothing says so rather than printing an empty table.
	var none strings.Builder
	if err := runModels([]string{"--publisher", "nobody"}, &none); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.String(), "no models match") {
		t.Fatalf("expected an empty-result message:\n%s", none.String())
	}
}

func TestRunModelsFit(t *testing.T) {
	// An explicit VRAM budget makes fit deterministic without any hardware probe.
	var b strings.Builder
	if err := runModels([]string{"--local", "--vram", "24"}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"FIT", "feasible", "24GB budget (given)", "recommended local model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fit output missing %q:\n%s", want, out)
		}
	}

	// A tiny budget pushes the larger local models over budget.
	var small strings.Builder
	if err := runModels([]string{"--local", "--vram", "1"}, &small); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small.String(), "over-budget") {
		t.Fatalf("a 1GB budget should leave a model over-budget:\n%s", small.String())
	}
}
