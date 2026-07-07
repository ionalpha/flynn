package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestKnownProviderSpec(t *testing.T) {
	for _, spec := range []string{"openai:gpt-5.5", "anthropic:claude-opus-4-8", "deepseek:", "gemini:x"} {
		if !knownProviderSpec(spec) {
			t.Errorf("knownProviderSpec(%q) = false, want true", spec)
		}
	}
	for _, spec := range []string{"", "openai", "qwen2.5:0.5b-instruct", "notaprovider:x"} {
		if knownProviderSpec(spec) {
			t.Errorf("knownProviderSpec(%q) = true, want false", spec)
		}
	}
}

// TestShellModelsShowsCatalog proves /models runs as a command in the full-screen session:
// the catalog reaches the scrollback and the input is never sent to the model as a turn.
// This is the regression guard for the commands only being wired into line mode.
func TestShellModelsShowsCatalog(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "TURN_SHOULD_NOT_RUN"})
	host.submit("/models", nil)
	waitIdle(t, host)

	tr := ui.transcript()
	if !strings.Contains(tr, "MODEL") || !strings.Contains(tr, "catalog v") {
		t.Fatalf("/models did not print the catalog to the scrollback:\n%s", tr)
	}
	if strings.Contains(tr, "TURN_SHOULD_NOT_RUN") {
		t.Error("/models leaked to the model as a turn instead of running as a command")
	}
}

// TestShellModelSwitches proves /model <spec> switches the session model, persists it as the
// default, and reports the switch in the scrollback, all without running a model turn.
func TestShellModelSwitches(t *testing.T) {
	// resolveModel builds a provider client but makes no network call; a dummy key lets a
	// hosted spec resolve.
	t.Setenv("OPENAI_API_KEY", "sk-test")
	host, ui := newHostForTest(t, constModel{text: "TURN_SHOULD_NOT_RUN"})
	// newREPL leaves dataDir unset; a real one lets the switch persist the default.
	host.s.dataDir = t.TempDir()

	host.submit("/model openai:gpt-5.5", nil)
	waitIdle(t, host)

	if host.s.modelSpec != "openai:gpt-5.5" {
		t.Fatalf("session modelSpec = %q, want openai:gpt-5.5", host.s.modelSpec)
	}
	tr := ui.transcript()
	if !strings.Contains(tr, "switched to openai:gpt-5.5") {
		t.Fatalf("/model switch not reported in the scrollback:\n%s", tr)
	}
	if strings.Contains(tr, "TURN_SHOULD_NOT_RUN") {
		t.Error("/model leaked to the model as a turn instead of running as a command")
	}
	if got, ok := readActiveModel(host.s.dataDir); !ok || got != "openai:gpt-5.5" {
		t.Fatalf("switch was not saved as the default: %q (ok=%v)", got, ok)
	}
}

func TestEffectiveModelSpecPrecedence(t *testing.T) {
	dir := t.TempDir()
	const builtIn = "anthropic:claude-opus-4-8"

	// Nothing saved: the built-in default is used.
	if got := effectiveModelSpec(builtIn, false, dir); got != builtIn {
		t.Fatalf("no default: got %q, want built-in %q", got, builtIn)
	}
	// A saved default is used when --model was not passed.
	if err := writeActiveModel(dir, "openai:gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if got := effectiveModelSpec(builtIn, false, dir); got != "openai:gpt-5.5" {
		t.Fatalf("saved default: got %q, want openai:gpt-5.5", got)
	}
	// An explicit --model overrides the saved default.
	if got := effectiveModelSpec("deepseek:chat", true, dir); got != "deepseek:chat" {
		t.Fatalf("explicit flag: got %q, want deepseek:chat", got)
	}
}

func TestModelsUseRecordsHostedDefault(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runModelUse([]string{"openai:gpt-5.5"}, dir, &out); err != nil {
		t.Fatalf("models use openai:gpt-5.5: %v", err)
	}
	got, ok := readActiveModel(dir)
	if !ok || got != "openai:gpt-5.5" {
		t.Fatalf("default = %q (ok=%v), want openai:gpt-5.5", got, ok)
	}
}

func TestModelsUseRejectsUnknown(t *testing.T) {
	if err := runModelUse([]string{"notaprovider:x"}, t.TempDir(), &bytes.Buffer{}); err == nil {
		t.Fatal("models use of an unknown id should error")
	}
}

func TestReplSwitchModelPersistsAndReports(t *testing.T) {
	// resolveModel builds a provider client but makes no network call; a dummy key lets a
	// hosted spec resolve.
	t.Setenv("OPENAI_API_KEY", "sk-test")
	dir := t.TempDir()
	var out bytes.Buffer
	s := &replSession{out: &out, dataDir: dir, modelSpec: "anthropic:claude-opus-4-8"}

	if err := s.switchModel(context.Background(), []string{"openai:gpt-5.5"}, &out); err != nil {
		t.Fatalf("switchModel: %v", err)
	}
	if s.modelSpec != "openai:gpt-5.5" {
		t.Fatalf("session modelSpec = %q, want openai:gpt-5.5", s.modelSpec)
	}
	if s.model == nil {
		t.Fatal("switchModel left the session model nil")
	}
	if got, ok := readActiveModel(dir); !ok || got != "openai:gpt-5.5" {
		t.Fatalf("switch was not saved as the default: %q (ok=%v)", got, ok)
	}
	if !strings.Contains(out.String(), "switched to openai:gpt-5.5") {
		t.Fatalf("no switch report:\n%s", out.String())
	}
}

func TestReplSwitchModelShowsCurrentWithNoArg(t *testing.T) {
	var out bytes.Buffer
	s := &replSession{out: &out, dataDir: t.TempDir(), modelSpec: "anthropic:claude-opus-4-8"}
	if err := s.switchModel(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "anthropic:claude-opus-4-8") {
		t.Fatalf("current model not shown:\n%s", out.String())
	}
}

func TestReplSwitchModelRejectsBadSpecWithoutChanging(t *testing.T) {
	var out bytes.Buffer
	s := &replSession{out: &out, dataDir: t.TempDir(), modelSpec: "anthropic:claude-opus-4-8"}
	if err := s.switchModel(context.Background(), []string{"notaprovider:x"}, &out); err == nil {
		t.Fatal("switching to an unknown provider should error")
	}
	if s.modelSpec != "anthropic:claude-opus-4-8" {
		t.Fatalf("a failed switch changed the session model to %q", s.modelSpec)
	}
	if _, ok := readActiveModel(s.dataDir); ok {
		t.Fatal("a failed switch wrote a default")
	}
}
