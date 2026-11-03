package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// briefFixture builds a planned goal with a two-item ledger, its first item proven.
func briefFixture(t *testing.T) (goal.Spec, goal.Status) {
	t.Helper()
	ledger, err := goal.AppendItems(nil,
		goal.LedgerItem{Item: "add the endpoint", Verify: "curl --fail localhost/health"},
		goal.LedgerItem{Item: "cover it with a test", Verify: "go test ./api/..."},
	)
	if err != nil {
		t.Fatal(err)
	}
	var st goal.Status
	st.Planned = true
	st.SyncLedger(ledger)
	if err := st.MarkProven(ledger[0].ID, "1", time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	return goal.Spec{Objective: "ship health checks", StopCondition: "it is shipped", Ledger: ledger}, st
}

// TestItemBriefNamesTheItemItsCheckAndWhereItSits: the run is handed one unit of work, the
// check it will be held to, and the plan around it. Stating the check up front is what lets
// a run work toward the thing that will actually be run rather than toward its own idea of
// done.
func TestItemBriefNamesTheItemItsCheckAndWhereItSits(t *testing.T) {
	spec, st := briefFixture(t)
	current, ok := st.CurrentItem(spec.Ledger)
	if !ok {
		t.Fatal("no current item on a half-settled ledger")
	}

	brief := itemBrief(spec, st, current)
	for _, want := range []string{
		"cover it with a test",    // the item
		"go test ./api/...",       // the check it will be held to
		"[x] 1. add the endpoint", // the settled item above it
		"[>] 2. cover it with a test",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief missing %q:\n%s", want, brief)
		}
	}
}

// TestTheBriefLandsAtItemBoundariesNotEveryTurn: re-stating the same item each step would
// copy it into the durable transcript once per turn for no new information. A changed item
// is exactly the moment the run has moved on and needs telling.
func TestTheBriefLandsAtItemBoundariesNotEveryTurn(t *testing.T) {
	spec, st := briefFixture(t)
	e := NewExecutor(llmtest.NewScripted(llmtest.SayText("working"), llmtest.SayText("still working")))
	r := ledgerGoal(t, spec, st)

	first, err := e.Execute(context.Background(), r)
	if err != nil {
		t.Fatalf("first step: %v", err)
	}
	if n := briefCount(t, first); n != 1 {
		t.Fatalf("the opening turn carried %d briefs, want 1", n)
	}

	st.Checkpoint = first
	second, err := e.Execute(context.Background(), ledgerGoal(t, spec, st))
	if err != nil {
		t.Fatalf("second step: %v", err)
	}
	if n := briefCount(t, second); n != 1 {
		t.Fatalf("a second turn on the same item carried %d briefs, want the one already given", n)
	}
}

// TestAFailingChecksDetailRidesIntoTheNextTurn: the agent is told what its own declared
// check reported rather than left to work out why the item is still open.
func TestAFailingChecksDetailRidesIntoTheNextTurn(t *testing.T) {
	spec, st := briefFixture(t)
	st.ItemFeedback = "`go test ./api/...` exited 1\n--- FAIL: TestHealth"
	e := NewExecutor(llmtest.NewScripted(llmtest.SayText("fixing")))

	raw, err := e.Execute(context.Background(), ledgerGoal(t, spec, st))
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	cp, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript(cp), "--- FAIL: TestHealth") {
		t.Fatalf("the failing check's detail did not reach the turn:\n%s", transcript(cp))
	}
}

// briefCount is how many times the current item's brief appears in a checkpoint's
// transcript, which is what tells a re-brief from the one the boundary earned.
func briefCount(t *testing.T, raw []byte) int {
	t.Helper()
	cp, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(transcript(cp), "Current item:")
}

func transcript(cp checkpoint) string {
	var b strings.Builder
	for _, m := range cp.Messages {
		for _, blk := range m.Blocks {
			if blk.Kind == llm.KindText {
				b.WriteString(blk.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// ledgerGoal builds the Goal resource Executor.Execute decodes, carrying a planned ledger
// and its per-item state.
func ledgerGoal(t *testing.T, spec goal.Spec, st goal.Status) resource.Resource {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := st.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "brief-run", Spec: raw, Status: enc}
}

// TestAFailingCheckIsReportedNotWorkedAround: the check is the model's own guess at what
// done looks like, so the turn that carries its failure also tells the run that a wrong
// check is to be said in the answer. Without it a run told "the check failed" reshapes
// the work until a mistaken check passes, which is the field failure this guards.
func TestAFailingCheckIsReportedNotWorkedAround(t *testing.T) {
	spec, st := briefFixture(t)
	st.ItemFeedback = "exit 1: findstr found nothing"
	e := NewExecutor(llmtest.NewScripted(llmtest.SayText("fixing")))

	raw, err := e.Execute(context.Background(), ledgerGoal(t, spec, st))
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	cp, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript(cp), "do not change the work to fit it") {
		t.Fatalf("the failing check's turn did not say a wrong check is to be reported:\n%s", transcript(cp))
	}
}

// TestTheRunHearsOnceThatItsPlanClosedWithItemsUnproven: when the last item settles by
// being abandoned there is no next item to brief, so the run is told once that its plan is
// over, which items were left unproven and why, and that its answer must say so.
func TestTheRunHearsOnceThatItsPlanClosedWithItemsUnproven(t *testing.T) {
	spec, st := briefFixture(t)
	// The conversation was last briefed on the second item, which has now been abandoned.
	cp := checkpoint{Item: spec.Ledger[1].ID}
	st.Ledger[1].Abandoned = true
	st.Ledger[1].AbandonedReason = "its check failed the same way on 2 consecutive runs: exit 1"

	briefTurn(&cp, spec, st)
	text := transcript(cp)
	for _, want := range []string{
		"Every planned item is settled",
		"[x] 1. add the endpoint",
		"[-] 2. cover it with a test",
		"cover it with a test: its check failed the same way on 2 consecutive runs: exit 1",
		"Say in your final answer",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("closing brief missing %q:\n%s", want, text)
		}
	}

	briefTurn(&cp, spec, st)
	if n := strings.Count(transcript(cp), "Every planned item is settled"); n != 1 {
		t.Fatalf("the closing brief appeared %d times, want once", n)
	}
}

// TestAFullyProvenPlanClosesWithoutANote: nothing was left unproven, so there is nothing
// to report and the run is not told anything extra.
func TestAFullyProvenPlanClosesWithoutANote(t *testing.T) {
	spec, st := briefFixture(t)
	if err := st.MarkProven(spec.Ledger[1].ID, "2", time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if brief := ledgerClosed(spec, st); brief != "" {
		t.Fatalf("a fully proven plan produced a closing brief: %q", brief)
	}
}

// doneOn is a finished conversation whose last brief was on item: the model's final answer
// is the last message, as it is when the reconciler holds that claim back.
func doneOn(t *testing.T, item string) []byte {
	t.Helper()
	raw, err := encodeCheckpoint(checkpoint{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "ship health checks"), llm.Text(llm.RoleAssistant, "Done.")},
		Done:     true,
		Result:   "Done.",
		Item:     item,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAHeldClaimReopensOnTheNextItem: the reconciler builds again after a completion claim
// only when it held the claim for an open item, so the executor must take the turn rather
// than return the finished conversation, and the turn must name the item that is next.
// Returning early here is how a run briefed on one item ended with the rest never tried.
func TestAHeldClaimReopensOnTheNextItem(t *testing.T) {
	spec, st := briefFixture(t)
	st.Checkpoint = doneOn(t, spec.Ledger[0].ID)
	model := llmtest.NewScripted(llmtest.SayText("on it"))

	raw, err := NewExecutor(model).Execute(context.Background(), ledgerGoal(t, spec, st))
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if model.Calls() != 1 {
		t.Fatalf("model called %d times, want the reopened turn", model.Calls())
	}
	sent := model.Requests()[0].Messages
	last := sent[len(sent)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.TextContent(), "cover it with a test") {
		t.Fatalf("the reopened turn did not brief the next item: %s %q", last.Role, last.TextContent())
	}
	cp, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cp.Done || cp.Result != "on it" {
		t.Fatalf("checkpoint done=%v result=%q, want the new turn's answer", cp.Done, cp.Result)
	}
}

// TestAReopenedTurnOnTheSameItemStillEndsOnTheUser: when the open item is the one the model
// was already briefed on and nothing new was reported, there is no brief to add, and a
// request ending on the model's own answer is one a provider refuses.
func TestAReopenedTurnOnTheSameItemStillEndsOnTheUser(t *testing.T) {
	spec, st := briefFixture(t)
	st.Checkpoint = doneOn(t, spec.Ledger[1].ID)
	model := llmtest.NewScripted(llmtest.SayText("on it"))

	if _, err := NewExecutor(model).Execute(context.Background(), ledgerGoal(t, spec, st)); err != nil {
		t.Fatalf("step: %v", err)
	}
	sent := model.Requests()[0].Messages
	if last := sent[len(sent)-1]; last.Role != llm.RoleUser || !strings.Contains(last.TextContent(), "The plan is not finished") {
		t.Fatalf("the reopened request ends on %s %q", last.Role, last.TextContent())
	}
}

// TestAFinishedPlanKeepsItsAnswer: with every item settled the claim stands, and a further
// step makes no model call.
func TestAFinishedPlanKeepsItsAnswer(t *testing.T) {
	spec, st := briefFixture(t)
	st.Ledger[1].Abandoned = true
	st.Checkpoint = doneOn(t, spec.Ledger[1].ID)
	model := llmtest.NewScripted()

	raw, err := NewExecutor(model).Execute(context.Background(), ledgerGoal(t, spec, st))
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if model.Calls() != 0 || string(raw) != string(st.Checkpoint) {
		t.Fatalf("a finished plan was advanced: %d call(s)", model.Calls())
	}
}
