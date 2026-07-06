package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	resources := store.Resources(reg)
	log := store.Log()

	// First run: block in the model call, then cancel to simulate the kill mid-step.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	var out1 bytes.Buffer
	_, runID, _, _ := drive(ctx1, &out1, blockingModel{}, harness.Plan{}, dir,
		"do the task", defaultSystemPrompt, resources, store.Jobs(), log, false, "", nil)
	if runID == "" {
		t.Fatal("no run id from the interrupted run")
	}

	// Resume with a model that finishes in one turn. This must converge quickly.
	finish := llmtest.NewScripted(llmtest.SayText("resumed and finished the task"))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	var out2 bytes.Buffer
	result, _, _, rerr := drive(ctx2, &out2, finish, harness.Plan{}, dir,
		"do the task", defaultSystemPrompt, resources, store.Jobs(), log, false, runID, nil)
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
