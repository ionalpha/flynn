package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// blockingModel blocks in Generate until the context is cancelled, standing in for a
// process killed while a model call is in flight: the run is interrupted mid-step, before
// the step's job is completed, so the job is left leased by the dead worker.
type blockingModel struct{}

func (blockingModel) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

// TestResumeContinuesInterruptedRun is the regression for the resume hang: a run is
// interrupted while a step is in flight (leaving the step job leased), then resumed. With
// startup orphan recovery the resume re-dispatches the orphaned step at once and drives
// the run to completion, rather than stalling for the full lease TTL. The test timeout is
// far below that TTL, so a regression to the old behavior fails here instead of hanging.
func TestResumeContinuesInterruptedRun(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	reg, err := governed.Registry()
	if err != nil {
		t.Fatal(err)
	}
	resources := store.Resources(reg)
	log := store.Log()

	// First run: block in the model call, then cancel to simulate the kill mid-step.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	var out1 bytes.Buffer
	_, runID, _, _ := driveRequest{
		out:       &out1,
		model:     blockingModel{},
		plan:      harness.Plan{},
		workdir:   dir,
		objective: "do the task",
		system:    defaultSystemPrompt,
		rstore:    resources,
		jq:        store.Jobs(),
		log:       log,
	}.execute(ctx1)
	if runID == "" {
		t.Fatal("no run id from the interrupted run")
	}

	// Resume with a model that finishes in one turn. This must converge quickly.
	finish := llmtest.NewScripted(llmtest.SayText("resumed and finished the task"))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	var out2 bytes.Buffer
	result, _, _, rerr := driveRequest{
		out:       &out2,
		model:     finish,
		plan:      harness.Plan{},
		workdir:   dir,
		objective: "do the task",
		system:    defaultSystemPrompt,
		rstore:    resources,
		jq:        store.Jobs(),
		log:       log,
		resumeID:  runID,
	}.execute(ctx2)
	if rerr != nil {
		t.Fatalf("resume returned an error: %v\nout:\n%s", rerr, out2.String())
	}
	if finish.Calls() == 0 {
		t.Fatalf("resume did not re-dispatch the interrupted step (the model was never called)\nout:\n%s", out2.String())
	}
	if !strings.Contains(result, "resumed and finished") {
		t.Fatalf("resume did not drive the run to completion; result = %q\nout:\n%s", result, out2.String())
	}
}

// TestResumeRebuildsTheRunsGuards assembles a goal run with every guard the goal command
// can set, then assembles its resumption the way `flynn resume` does, and requires the two
// postures to match. A resume used to come back with none of them: no item verifier, so
// its first verify step failed the run with goal_no_verifier, and no spend cap, approval
// gate or outside-action gate.
func TestResumeRebuildsTheRunsGuards(t *testing.T) {
	ctx := t.Context()
	store := memStore(t)
	reg, err := governed.Registry()
	if err != nil {
		t.Fatal(err)
	}
	base := driveRequest{
		out: &bytes.Buffer{}, model: llmtest.NewScripted(llmtest.SayText("done")), workdir: t.TempDir(),
		objective: "do the task", system: defaultSystemPrompt, rstore: store.Resources(reg), jq: store.Jobs(), log: store.Log(),
	}
	first, err := base.withOptions(
		withShellPolicy(checkShell), withPlanning(), withLedgerProof(),
		withBudget(budgetpkg.Limits{Tokens: 400_000, Cost: 0.5}),
		withApproval([]string{"bash"}, nil), withAllowance([]string{"write"}, nil),
	).assembly().assemble(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := first.Posture()
	_ = first.Close()

	resumed := base
	resumed.resumeID = first.ID()
	resumed, err = resumed.resuming(ctx, t.TempDir(), checkShell)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resumed.assembly().assemble(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if got := second.Posture(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the resumed run's posture\n%+v\nwant the original run's\n%+v", got, want)
	}
}

// TestResumeGuardsRefusesAnExternalAgentRun: resume drives a native model, so continuing a
// run an external agent CLI drove would hand it to a different driver.
func TestResumeGuardsRefusesAnExternalAgentRun(t *testing.T) {
	if _, _, err := resumeGuards(governed.Posture{Mode: governed.ModeExternal}); err == nil {
		t.Fatal("resumeGuards accepted an external agent's run")
	}
	opts, fanout, err := resumeGuards(governed.Posture{Mode: governed.ModeFanout, Verifier: true})
	if err != nil || !fanout {
		t.Fatalf("a fan-out run: fanout %v, err %v", fanout, err)
	}
	var c driveConfig
	for _, o := range opts {
		o(&c)
	}
	if c.planning {
		t.Error("a fan-out resume turned on the single run's planning phase")
	}
}
