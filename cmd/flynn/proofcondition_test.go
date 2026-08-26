package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// The promotion condition for --require-proof is a measurement, and this is where it is
// taken, on whichever platform the suite is running on: a planned goal whose item carries
// a check that exits 0, driven through the assembly the binary uses, with the verdict read
// off the goal the run leaves behind.
//
// The condition asks whether the refusal would cost an honest run anything. It costs
// nothing where the item's check executes and proves it, and it costs the whole run where
// the check cannot be executed at all: a term or an item's check is model-authored, so it
// is dispatched as semi-trusted work, and a host whose sandbox offers only a process jail
// refuses it before it runs. On such a host no item can ever be proven, and the refusal
// would stop every planned run for a reason that says nothing about the run.
//
// So the assertion is the implication rather than the outcome. What the host can contain
// decides which answer is correct, and the answer that is never correct on either host is
// an item marked proven with no executed check behind it.
func TestThePromotionConditionForLedgerProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))
	// The plan is the model's, in the shape the planner asks for, and its check is a
	// real command: a plan whose clause is prose would measure the model auditor rather
	// than the producer, and prove nothing about whether checks can run here.
	model := llmtest.NewScripted(
		llmtest.SayText(`[{"item":"state the answer","verify":"exit 0"}]`),
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"answer.txt","content":"42"}`)),
		llmtest.SayText("done"),
	)
	run, err := assembleMission(model, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
		rstore, store.Jobs(), store.Log(), nil, "", sandbox.ResourceLimits{}, true, false, gateSetup{})
	if err != nil {
		t.Fatalf("assemble the mission the binary assembles: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	go func() { _ = run.rt.Start(ctx) }()

	if _, err := run.sess.Submit(ctx, run.rt, goal.Spec{
		Objective:     "state the answer",
		StopCondition: "the objective is fully accomplished",
	}); err != nil {
		t.Fatalf("submit a goal to plan: %v", err)
	}
	if _, err := run.sess.Wait(ctx); err != nil {
		t.Fatalf("the planned run did not settle: %v", err)
	}

	r, err := rstore.Get(ctx, goal.Kind, resource.Scope{}, run.sess.ID())
	if err != nil {
		t.Fatalf("get the goal the run left behind: %v", err)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatalf("decode the goal's spec: %v", err)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode the goal's status: %v", err)
	}
	if len(spec.Ledger) != 1 || spec.Ledger[0].Verify != "exit 0" {
		t.Fatalf("the run planned no ledger to measure: %+v", spec.Ledger)
	}

	proven := len(status.Ledger) == 1 && status.Ledger[0].Proven
	contained := sandbox.ContainmentOf(run.parts.sandbox) >= sandbox.Required(sandbox.TrustSemi)

	switch {
	case contained && !proven:
		// The condition failing on a host that can run the check is the finding the
		// staged rollout is waiting for: the producer had everything it needed and the
		// item still did not settle.
		t.Fatalf("a host that can contain the check left the item unproven, so the refusal would stop an honest run here: %+v", status.Ledger)
	case !contained && proven:
		t.Fatalf("an item was marked proven on a host that cannot run its check: %+v", status.Ledger)
	case !contained:
		// No item can be proven here, so the default cannot be on here. The run has to
		// say why rather than leaving the item silently unsettled.
		if !strings.Contains(status.ItemFeedback, "check") && !strings.Contains(status.Message, "check") {
			t.Fatalf("an item that could not be checked was left unsettled with no reason on the record: feedback %q, message %q",
				status.ItemFeedback, status.Message)
		}
	}
}
