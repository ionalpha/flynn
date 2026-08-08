package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

func goalResource(t *testing.T, spec goal.Spec) resource.Resource {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw}
}

// TestPlannerExpandsObjective: the planner turns the model's JSON array into ledger
// items, carrying each item's text and its verify clause, and leaving the id unset for
// the ledger to content-address on append.
func TestPlannerExpandsObjective(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText(
		`[{"item":"add a /health endpoint","verify":"curl localhost:8080/health returns 200"},` +
			`{"item":"cover it with a test","verify":"go test ./server passes"}]`))
	p := NewPlanner(model)

	items, err := p.Plan(context.Background(), goalResource(t, goal.Spec{Objective: "ship a health check", StopCondition: "endpoint live and tested"}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Item != "add a /health endpoint" || items[0].Verify != "curl localhost:8080/health returns 200" {
		t.Fatalf("item 0 = %+v", items[0])
	}
	if items[0].ID != "" {
		t.Fatalf("planner assigned an id (%q); the ledger content-addresses on append", items[0].ID)
	}

	// The call carried the planning system prompt and the objective, not the build loop's.
	req := model.Requests()[0]
	if !strings.Contains(req.System, "planning phase") {
		t.Fatalf("planner did not use its own system prompt: %q", req.System)
	}
	if body := req.Messages[0].TextContent(); !strings.Contains(body, "ship a health check") || !strings.Contains(body, "endpoint live and tested") {
		t.Fatalf("planner prompt missing the objective or stop condition: %q", body)
	}
}

// TestPlannerEmptyPlanIsNotAnError pins the Planner contract: a model that finds
// nothing to plan returns no items and no error, so the reconciler settles the goal as
// a stall with a reason rather than the planner failing and retrying.
func TestPlannerEmptyPlanIsNotAnError(t *testing.T) {
	p := NewPlanner(llmtest.NewScripted(llmtest.SayText(`[]`)))
	items, err := p.Plan(context.Background(), goalResource(t, goal.Spec{Objective: "already done", StopCondition: "c"}))
	if err != nil {
		t.Fatalf("empty plan errored: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d items, want 0", len(items))
	}
}

// TestPlannerToleratesFenceAndProse: a model that wraps its array in a code fence or a
// sentence still plans, because the parser extracts the outermost JSON array.
func TestPlannerToleratesFenceAndProse(t *testing.T) {
	p := NewPlanner(llmtest.NewScripted(llmtest.SayText(
		"Here is the plan:\n```json\n[{\"item\":\"do a\",\"verify\":\"check a\"}]\n```\n")))
	items, err := p.Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 || items[0].Item != "do a" {
		t.Fatalf("got %+v, want one item 'do a'", items)
	}
}

// TestPlannerBadOutputIsTransient: output with no JSON array, or an item missing its
// verify clause, is a malformed plan reported transient, so the retry ladder samples the
// model again before the goal stalls rather than stalling on the first bad turn.
func TestPlannerBadOutputIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name, out string
	}{
		{"no array", "I could not work out a plan."},
		{"item missing verify", `[{"item":"do a","verify":""}]`},
		{"item missing text", `[{"item":"","verify":"check a"}]`},
		{"not an array", `{"item":"do a","verify":"check a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlanner(llmtest.NewScripted(llmtest.SayText(tc.out)))
			_, err := p.Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
			if err == nil {
				t.Fatal("expected an error for malformed planner output")
			}
			if fault.Classify(err) != fault.Transient {
				t.Fatalf("class = %q, want transient so the plan step retries", fault.Classify(err))
			}
		})
	}
}

// TestPlannerIsShownTheExistingLedger pins the re-plan decision: when the goal already
// carries a ledger, the planner is shown it and told to return only what is not already
// covered. This is what lets a re-planned goal return nothing new from the model's side,
// on top of the append rule's structural backstop.
func TestPlannerIsShownTheExistingLedger(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText(`[]`))
	p := NewPlanner(model)
	spec := goal.Spec{
		Objective:     "o",
		StopCondition: "c",
		Ledger: []goal.LedgerItem{
			{ID: goal.ItemID("already planned work", "already checked"), Item: "already planned work", Verify: "already checked"},
		},
	}
	if _, err := p.Plan(context.Background(), goalResource(t, spec)); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	body := model.Requests()[0].Messages[0].TextContent()
	if !strings.Contains(body, "already planned work") {
		t.Fatalf("planner was not shown the existing ledger: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "only items that are not already covered") {
		t.Fatalf("planner was not told to return only new items: %q", body)
	}
}

var _ llm.Model = (*llmtest.ScriptedModel)(nil)
