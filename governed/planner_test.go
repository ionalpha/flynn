package governed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// TestThePlannerKnowsTheRunsToolNames: the planner built for a run is told the run's own tool
// names, so a plan whose check calls one is sent back to the model and not recorded.
func TestThePlannerKnowsTheRunsToolNames(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText(`[{"item":"read it","verify":"zz_tool_not_a_program x"}]`),
		llmtest.SayText(`[{"item":"read it","verify":"go version"}]`),
	)
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c"})
	if err != nil {
		t.Fatal(err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw}

	items, err := planner(model, []mission.Tool{namedTool("zz_tool_not_a_program")}).Plan(context.Background(), r)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 || items[0].Verify != "go version" {
		t.Fatalf("items = %+v, want the corrected check", items)
	}
	if got := model.Requests()[0].System; !strings.Contains(got, "zz_tool_not_a_program") {
		t.Fatalf("the planning prompt does not carry the run's tool names: %q", got)
	}
}
