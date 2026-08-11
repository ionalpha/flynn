package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// driveMarkedRun drives a run whose model tries to write a file, with the write tool marked
// as reaching outside the workspace and declared or not, and returns the goal's settled
// status and the working directory.
//
// It goes through the same two lists an operator passes on the command line, so what the
// test exercises is the composition rather than a hand-built gate: the marking installs the
// gate, and the declaration is turned into what the goal carries.
func driveMarkedRun(t *testing.T, marked, declared []string) (goal.Status, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
		llmtest.SayText("done"),
	)
	run, err := assembleMission(model, harness.Plan{}, dir, defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), nil, "", sandbox.ResourceLimits{}, false, false,
		gateSetup{outside: marked})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	done := make(chan struct{})
	go func() { _ = run.rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if _, err := run.rt.SubmitGoal(ctx, run.sess.ID(), goal.Spec{
		Objective:     "write hello.txt",
		StopCondition: "the file is written",
		Allowances:    declaredAllowances(declared),
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitForSettled(ctx, t, rstore, run.sess.ID())

	r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, run.sess.ID())
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st, dir
}

// TestAnUndeclaredIrreversibleActionPausesTheRun is the whole feature end to end, through
// the binary's own assembly: the objective asked for the file, nobody declared the action
// that writes it, the file is not there, and the run is stopped with the ask rather than
// carrying on to find another way to satisfy the objective.
func TestAnUndeclaredIrreversibleActionPausesTheRun(t *testing.T) {
	st, dir := driveMarkedRun(t, []string{"write"}, nil)

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err == nil {
		t.Fatal("the undeclared action wrote its file anyway")
	}
	if st.Phase != goal.PhaseStalled {
		t.Fatalf("phase = %q, want %q", st.Phase, goal.PhaseStalled)
	}
	if !strings.Contains(st.Message, "write") {
		t.Errorf("message %q does not name the action to declare", st.Message)
	}
	if !strings.Contains(st.Message, "declare an allowance") {
		t.Errorf("message %q does not tell its reader how to answer it", st.Message)
	}
}

// TestADeclaredIrreversibleActionRunsToCompletion is the same run with the authority
// written down in advance, which is the only thing that changes.
func TestADeclaredIrreversibleActionRunsToCompletion(t *testing.T) {
	st, dir := driveMarkedRun(t, []string{"write"}, []string{"write"})

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("the declared action did not run: %v", err)
	}
	if st.Phase == goal.PhaseStalled {
		t.Fatalf("a declared run was paused anyway: %q", st.Message)
	}
}

// TestMarkingNothingLeavesTheRunAsItWas is the default every existing run is on.
func TestMarkingNothingLeavesTheRunAsItWas(t *testing.T) {
	st, dir := driveMarkedRun(t, nil, nil)

	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatalf("an unmarked run did not write its file: %v", err)
	}
	if st.Phase == goal.PhaseStalled {
		t.Fatalf("an unmarked run was paused: %q", st.Message)
	}
}

// TestDeclaredAllowancesNormalizesTheOperatorsList: a blank entry is not an action, and
// declaring one action twice is one authorization rather than two.
func TestDeclaredAllowancesNormalizesTheOperatorsList(t *testing.T) {
	got := declaredAllowances([]string{"deploy", " ", "deploy", "secret.release"})
	want := []goal.Allowance{{Action: "deploy"}, {Action: "secret.release"}}
	if len(got) != len(want) {
		t.Fatalf("allowances = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allowance %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(declaredAllowances(nil)) != 0 {
		t.Error("declaring nothing produced an allowance")
	}
}

// TestOutsideGateIsAbsentWhenNothingIsMarked keeps the fan-out's base spec honest: a run
// with no marked action carries no gate rather than one that is installed and marks
// nothing. Both admit everything; only one of them claims a gate is there.
func TestOutsideGateIsAbsentWhenNothingIsMarked(t *testing.T) {
	if got := outsideGate(nil); got != nil {
		t.Errorf("gate = %v, want none when nothing is marked", got)
	}
	if got := outsideGate([]string{" "}); got != nil {
		t.Errorf("gate = %v, want none when only blanks are marked", got)
	}
	if outsideGate([]string{"deploy"}) == nil {
		t.Error("no gate was built for a marked action")
	}
}

// TestWithAllowanceSetsBothHalves keeps the two lists together on the run's config. Neither
// means anything alone: marking with nothing declared is a run that stops the first time it
// reaches one, and declaring an action nothing marked authorizes what was never gated.
func TestWithAllowanceSetsBothHalves(t *testing.T) {
	var cfg driveConfig
	withAllowance([]string{"write"}, declaredAllowances([]string{"write"}))(&cfg)

	if len(cfg.gates.outside) != 1 || cfg.gates.outside[0] != "write" {
		t.Fatalf("marked = %v, want [write]", cfg.gates.outside)
	}
	if len(cfg.allowances) != 1 || cfg.allowances[0].Action != "write" {
		t.Fatalf("declared = %+v, want one declaration of write", cfg.allowances)
	}
	if len(cfg.gates.allowanceOptions()) != 1 {
		t.Error("a marked action installed no gate")
	}
}
