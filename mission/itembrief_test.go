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
