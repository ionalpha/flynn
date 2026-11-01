package mission

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
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

// toolPlanner is a planner that knows the run's tool names, on a host where a lookup finds
// only the programs in have, and whose OS is goos.
func toolPlanner(model llm.Model, goos string, have ...string) *Planner {
	p := NewPlanner(model, WithPlannerTools("skill_read", "bash", "read"))
	p.goos = goos
	p.lookPath = func(name string) (string, error) {
		if slices.Contains(have, name) {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	return p
}

// TestPlannerRefusesACheckThatCallsAToolAndSaysWhy: the field failure. The model planned
// `skill_read systematic-debugging >NUL` as a check; a shell cannot start a tool, so the check
// could only report the program missing and the goal stalled on it. The planner sends the
// plan back with the reason, and takes the corrected plan.
func TestPlannerRefusesACheckThatCallsAToolAndSaysWhy(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.SayText(`[{"item":"read the debugging skill","verify":"skill_read systematic-debugging >NUL"}]`),
		llmtest.SayText(`[{"item":"read the debugging skill","verify":"findstr /c:\"hypothesis\" notes.md"}]`),
	)
	items, err := toolPlanner(model, "windows").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 || strings.Contains(items[0].Verify, "skill_read") {
		t.Fatalf("items = %+v, want the corrected check", items)
	}

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2 (plan, then the correction)", len(reqs))
	}
	msgs := reqs[1].Messages
	if len(msgs) != 3 || msgs[1].Role != llm.RoleAssistant || msgs[2].Role != llm.RoleUser {
		t.Fatalf("correction turn shape = %+v, want the first request, the model's plan, then the refusal", msgs)
	}
	refusal := msgs[2].TextContent()
	for _, want := range []string{"skill_read", "tool", "recorded result"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("refusal does not say %q: %q", want, refusal)
		}
	}
	if !strings.Contains(reqs[0].System, "skill_read") {
		t.Fatalf("the planning prompt does not name the tools that are not programs: %q", reqs[0].System)
	}
}

// TestPlannerRefusesAPosixShellOnWindows: `bash -c` exited 256 for every check in a field run,
// because Git's bash cannot start under the confined token. The tool of that name is refused
// as a check there even though a program called bash is on the path.
func TestPlannerRefusesAPosixShellOnWindows(t *testing.T) {
	bad := `[{"item":"i","verify":"bash -c \"echo hi\" > out.log 2>&1"}]`
	good := `[{"item":"i","verify":"findstr hi out.log"}]`

	model := llmtest.NewScripted(llmtest.SayText(bad), llmtest.SayText(good))
	if _, err := toolPlanner(model, "windows", "bash").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"})); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if n := len(model.Requests()); n != 2 {
		t.Fatalf("model called %d times on windows, want 2: bash must be refused there", n)
	}

	model = llmtest.NewScripted(llmtest.SayText(bad))
	items, err := toolPlanner(model, "linux", "bash").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil || len(items) != 1 || len(model.Requests()) != 1 {
		t.Fatalf("a real bash on linux was refused: items=%v err=%v calls=%d", items, err, len(model.Requests()))
	}
}

// TestPlannerAllowsAToolNameThatIsAlsoAProgram: grep, read and the like are programs on many
// hosts, and a check that runs the real one is fine. Only a name the host does not have is
// refused.
func TestPlannerAllowsAToolNameThatIsAlsoAProgram(t *testing.T) {
	plan := `[{"item":"i","verify":"read x"}]`
	model := llmtest.NewScripted(llmtest.SayText(plan))
	items, err := toolPlanner(model, "linux", "read").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil || len(items) != 1 {
		t.Fatalf("a check running a program the host has was refused: items=%v err=%v", items, err)
	}

	model = llmtest.NewScripted(llmtest.SayText(plan), llmtest.SayText(plan), llmtest.SayText(plan))
	if _, err = toolPlanner(model, "linux").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"})); err == nil {
		t.Fatal("a check running a name the host lacks was accepted")
	}
}

// TestPlannerThatKeepsNamingATool is transient after the correction budget: the retry ladder
// samples the model again, and the goal does not record a check that cannot run.
func TestPlannerThatKeepsNamingATool(t *testing.T) {
	plan := `[{"item":"i","verify":"skill_read x"}]`
	model := llmtest.NewScripted(llmtest.SayText(plan), llmtest.SayText(plan), llmtest.SayText(plan))
	_, err := toolPlanner(model, "windows").Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err == nil {
		t.Fatal("expected an error when the model kept planning a tool as a check")
	}
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("class = %q, want transient", fault.Classify(err))
	}
	if n := len(model.Requests()); n != 1+plannerCorrections {
		t.Fatalf("model called %d times, want %d (the plan and %d corrections)", n, 1+plannerCorrections, plannerCorrections)
	}
}

// TestPlannerWithoutToolsAcceptsAnyCheck keeps the planner's old behaviour for a caller that
// does not say what the tools are.
func TestPlannerWithoutToolsAcceptsAnyCheck(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText(`[{"item":"i","verify":"skill_read x"}]`))
	items, err := NewPlanner(model).Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"}))
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
}

var _ llm.Model = (*llmtest.ScriptedModel)(nil)
