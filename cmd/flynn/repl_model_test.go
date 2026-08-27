package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestSwitchModelDrivesTheNewModelAndSavesIt proves /model does the two things it claims:
// the rest of the session drives the model named, and the choice becomes the default a
// later launch opens on. A session-only switch would leave the operator retyping it every
// run, and a saved-but-not-applied one would report a model the turns do not use.
func TestSwitchModelDrivesTheNewModelAndSavesIt(t *testing.T) {
	noProviderKeys(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	s, buf := newSlashSession(t, llmtest.NewScripted())
	s.learnEnabled = true

	if err := s.switchModel(context.Background(), []string{"openai:gpt-5-mini"}, s.out); err != nil {
		t.Fatalf("switchModel: %v\n%s", err, buf.String())
	}
	if s.modelSpec != "openai:gpt-5-mini" {
		t.Fatalf("the session still drives %s", s.modelSpec)
	}
	if s.distiller == nil {
		t.Fatal("a learning session lost its distiller across the switch, so it would stop learning back")
	}
	if saved, ok := readActiveModel(s.dataDir); !ok || saved != "openai:gpt-5-mini" {
		t.Fatalf("the default records %q (set: %v), want the model just switched to", saved, ok)
	}
	if !strings.Contains(buf.String(), "switched to openai:gpt-5-mini") {
		t.Fatalf("the switch was not reported:\n%s", buf.String())
	}
}

// TestSwitchModelHoldsWhenTheDefaultCannotBeSaved proves the two halves are independent:
// a data directory that cannot be written to loses the saved default, and the session
// still drives the model asked for. Failing the switch outright would refuse a change
// that had in fact already taken.
func TestSwitchModelHoldsWhenTheDefaultCannotBeSaved(t *testing.T) {
	noProviderKeys(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	s, buf := newSlashSession(t, llmtest.NewScripted())
	// A directory where the recorded default belongs. The data directory itself stays
	// usable, so the model still resolves and only the saving fails, which is the state
	// this is about; on every platform a directory cannot be written to as a file.
	if err := os.MkdirAll(activeModelPath(s.dataDir), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := s.switchModel(context.Background(), []string{"openai:gpt-5-mini"}, s.out); err != nil {
		t.Fatalf("switchModel: %v\n%s", err, buf.String())
	}
	if s.modelSpec != "openai:gpt-5-mini" {
		t.Fatalf("the session still drives %s, so the switch was lost with the default", s.modelSpec)
	}
	if !strings.Contains(buf.String(), "could not save it as the default") {
		t.Fatalf("the session did not say the default was not saved:\n%s", buf.String())
	}
}

// TestSwitchModelRefusesAHarnessSwapMidRun proves a run that has already recorded turns
// cannot be handed to an external agent. A record declares the one harness that drove the
// run, so a swap halfway through would seal a declaration true of only part of it. The
// message names what is driving now, which for a native session is its model spec.
func TestSwitchModelRefusesAHarnessSwapMidRun(t *testing.T) {
	s, _ := newSlashSession(t, llmtest.NewScripted())
	s.started = true

	err := s.switchModel(context.Background(), []string{"claude:sonnet"}, s.out)
	if err == nil {
		t.Fatal("a started run was handed to an external harness")
	}
	if !strings.Contains(err.Error(), s.modelSpec) {
		t.Fatalf("the refusal does not name what is driving the run: %v", err)
	}
	if s.modelSpec == "claude:sonnet" {
		t.Fatal("the refused switch was applied anyway")
	}
}

// TestSwitchModelReportsAnUnresolvableSpec proves a spec that resolves to nothing (an
// unknown provider, a missing key) is reported and the session keeps driving what it was
// driving. Ending the session over a typo would cost the conversation.
func TestSwitchModelReportsAnUnresolvableSpec(t *testing.T) {
	noProviderKeys(t)
	s, _ := newSlashSession(t, llmtest.NewScripted())

	if err := s.switchModel(context.Background(), []string{"notaprovider:x"}, s.out); err == nil {
		t.Fatal("an unresolvable spec became the session's model")
	}
	if s.modelSpec != "openai:gpt-5.5" {
		t.Fatalf("the session moved off its model to %s", s.modelSpec)
	}
}

// TestSwitchModelReportsAnUnavailableHarness proves a switch to an external agent whose
// CLI is not installed is reported and the session keeps driving what it had. This is the
// one /model failure the user is most likely to hit: the backend name is right and the
// tool is simply not there.
func TestSwitchModelReportsAnUnavailableHarness(t *testing.T) {
	withNoExecutables(t)
	s, _ := newSlashSession(t, llmtest.NewScripted())

	err := s.switchModel(context.Background(), []string{"claude:sonnet"}, s.out)
	if err == nil {
		t.Fatal("a session switched onto a harness that is not installed")
	}
	if !strings.Contains(err.Error(), "/model claude:sonnet") {
		t.Fatalf("the failure does not name the command that caused it: %v", err)
	}
	if s.modelSpec != "openai:gpt-5.5" || s.ext != nil {
		t.Fatalf("the failed switch moved the session onto %s (external: %v)", s.modelSpec, s.ext != nil)
	}
}

// withNoExecutables empties the executable search path for the test, so a harness CLI is
// deterministically absent whether or not the machine running the tests has one installed.
func withNoExecutables(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}
