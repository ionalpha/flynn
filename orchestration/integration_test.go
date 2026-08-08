package orchestration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/orchestration"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// fanoutModel is a stateless content-routing fake: the parent run fans out two
// children on its first turn and reports completion once their results are folded
// back; a child run answers directly. Stateless so the parent and both children can
// call it concurrently.
type fanoutModel struct{ parentObjective string }

func (m fanoutModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	first := firstUserText(req.Messages)
	if strings.Contains(first, m.parentObjective) {
		if hasToolResult(req.Messages) {
			return llmtest.SayText("delegated work complete"), nil
		}
		return twoSpawns(), nil
	}
	return llmtest.SayText("result for " + first), nil
}

func firstUserText(msgs []llm.Message) string {
	for _, msg := range msgs {
		if msg.Role == llm.RoleUser {
			if t := msg.TextContent(); t != "" {
				return t
			}
		}
	}
	return ""
}

func hasToolResult(msgs []llm.Message) bool {
	for _, msg := range msgs {
		for _, b := range msg.Blocks {
			if b.Kind == llm.KindToolResult {
				return true
			}
		}
	}
	return false
}

func twoSpawns() llm.Response {
	mk := func(id, obj string) llm.Block {
		in, _ := json.Marshal(mission.SubGoal{Objective: obj, Actions: []string{mission.ActionModelGenerate}})
		return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: mission.ActionSpawn, Input: in}}
	}
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{mk("s1", "research alpha"), mk("s2", "research beta")}},
		StopReason: llm.StopToolUse,
	}
}

// TestFanoutEndToEnd is the whole thing: a parent goal, driven through a real
// runtime, fans out two child goals via the spawner; each child runs as its own
// governed goal (owned by the parent, grant narrowed to what it was delegated),
// converges, and its result is folded back so the parent converges. This exercises
// the spawn tool, the spawner's child creation, concurrent child execution on the
// reconcile loop, and the fold, with no fakes below the model.
func TestFanoutEndToEnd(t *testing.T) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	q := jobs.NewMemory()
	sp := orchestration.NewSpawner(store, nil, orchestration.WithConcurrency(8))

	// No executor default grant: each goal carries its own grant (the parent's is set
	// on the goal below; children get a narrowed one), so the per-goal grant governs.
	exec := mission.NewExecutor(
		fanoutModel{parentObjective: "delegate"},
		mission.WithFanout(sp),
	)
	rt, err := runtime.New(runtime.Config{
		Store:        store,
		Jobs:         q,
		Executor:     exec,
		Stop:         mission.Convergence{},
		PollInterval: 20 * time.Millisecond,
		WorkerPoll:   10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Break the construction cycle: the spawner hands children to the runtime that
	// holds the executor that holds the spawner.
	sp.SetEnqueue(func(ctx context.Context, key resource.Key) error {
		_, rerr := rt.Resume(ctx, key.Name)
		return rerr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if _, err := rt.SubmitGoal(ctx, "root", goal.Spec{
		Objective:     "delegate two research tasks",
		StopCondition: "both delegated tasks are complete",
		Grant:         []string{"spawn", mission.ActionModelGenerate},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The parent converges only after both children have run and folded back.
	parent := waitForPhase(ctx, t, store, "root", goal.PhaseConverged)
	if msg := decodeMessage(t, parent); !strings.Contains(msg, "complete") {
		t.Fatalf("parent result = %q, want a completion message", msg)
	}

	// Two children exist, each owned by the parent and narrowed to the delegated
	// action only (the spawn capability did not flow to them).
	children := childrenOf(ctx, t, store, "root")
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	for _, c := range children {
		cs, err := goal.DecodeSpec(c)
		if err != nil {
			t.Fatal(err)
		}
		if cs.Depth != 1 {
			t.Fatalf("child depth = %d, want 1", cs.Depth)
		}
		if len(cs.Grant) != 1 || cs.Grant[0] != mission.ActionModelGenerate {
			t.Fatalf("child grant = %v, want [%s] (spawn did not flow down)", cs.Grant, mission.ActionModelGenerate)
		}
	}
}

func waitForPhase(ctx context.Context, t *testing.T, s resource.Store, name string, want goal.Phase) resource.Resource {
	t.Helper()
	// Poll a bounded number of times rather than against a wall clock (the lint floor
	// forbids time.Now); ~800 * 15ms covers the ctx timeout.
	for range 800 {
		if r, err := s.Get(ctx, goal.Kind, resource.Scope{}, name); err == nil {
			if st, derr := goal.DecodeStatus(r); derr == nil && st.Phase == want {
				return r
			}
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("goal %q did not reach %s within the deadline", name, want)
	return resource.Resource{}
}

func childrenOf(ctx context.Context, t *testing.T, s resource.Store, parent string) []resource.Resource {
	t.Helper()
	all, err := s.List(ctx, goal.Kind, resource.Scope{}, resource.Everything())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var kids []resource.Resource
	for _, r := range all {
		for _, o := range r.OwnerReferences {
			if o.Name == parent && o.Controller {
				kids = append(kids, r)
			}
		}
	}
	return kids
}

func decodeMessage(t *testing.T, r resource.Resource) string {
	t.Helper()
	st, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	return st.Message
}

// --- plan-driven fan-out, end to end ------------------------------------------

// alwaysDone is a model whose every turn is a final one. Plan-driven fan-out puts the
// decomposition on the spec rather than in the conversation, so nothing here needs a
// model that decides to delegate: what is under test is whether the graph runs.
type alwaysDone struct{}

func (alwaysDone) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llmtest.SayText("the unit's work is done"), nil
}

// passingVerifier reports every item's declared check as run and passing, which is what
// puts a real verification on each child's record for the evidence gate to consume. It
// stands in for a command runner, not for the gate: the gate, the record and the
// settlement are all the shipped ones.
type passingVerifier struct{}

func (passingVerifier) VerifyItem(context.Context, resource.Resource, goal.LedgerItem) (goal.ItemVerdict, error) {
	return goal.ItemVerdict{Passed: true, Executed: true, Output: "ok"}, nil
}

// TestUnitGraphEndToEnd is F17 whole: a goal whose spec carries a unit graph with a
// dependency edge runs those units as governed children through a real runtime, each
// child proves its own ledger item through the shipped evidence gate, each unit settles
// from that proof, and the parent converges only once the graph has. Nothing below the
// model and the command runner is a fake.
func TestUnitGraphEndToEnd(t *testing.T) {
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	q := jobs.NewMemory()
	sp := orchestration.NewSpawner(store, nil, orchestration.WithConcurrency(4))
	exec := mission.NewExecutor(alwaysDone{}, mission.WithFanout(sp))

	rt, err := runtime.New(runtime.Config{
		Store:    store,
		Jobs:     q,
		Executor: exec,
		Stop:     mission.Convergence{},
		// The plan path is governed through the executor's own waist, so a unit spawn is
		// admitted against the goal's grant exactly as the spawn tool is.
		Units:              orchestration.Units(sp, exec.Dispatcher()),
		Verifier:           passingVerifier{},
		Evidence:           evidence.NewSpineEvidence(spine.NewMemoryLog()),
		RequireLedgerProof: true,
		PollInterval:       20 * time.Millisecond,
		WorkerPoll:         10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sp.SetEnqueue(func(ctx context.Context, key resource.Key) error {
		_, rerr := rt.Resume(ctx, key.Name)
		return rerr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	units := []goal.Unit{
		{
			ID: "parser", Objective: "build the parser", Verify: "go test ./parser",
			Actions: []string{mission.ActionModelGenerate},
		},
		{
			ID: "api", Objective: "build the api on the parser", Verify: "go test ./api",
			DependsOn: []string{"parser"}, Actions: []string{mission.ActionModelGenerate},
		},
	}
	if _, err := rt.SubmitGoal(ctx, "root", goal.Spec{
		Objective:     "deliver the parser and the api",
		StopCondition: "both units are proven",
		Grant:         []string{mission.ActionSpawn, mission.ActionModelGenerate},
		Units:         units,
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	parent := waitForPhase(ctx, t, store, "root", goal.PhaseConverged)
	status, err := goal.DecodeStatus(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !status.UnitsSettled(units) {
		t.Fatalf("the parent converged over an unsettled graph: %+v", status.Units)
	}
	for _, st := range status.Units {
		if !st.Proven {
			t.Fatalf("unit %s settled unproven: %+v", st.ID, st)
		}
		if st.Evidence == "" {
			t.Fatalf("unit %s was proven with nothing behind it: %+v", st.ID, st)
		}
		if want := orchestration.UnitChildName("root", st.ID); st.ChildID != want {
			t.Fatalf("unit %s ran as %q, want the derived %q", st.ID, st.ChildID, want)
		}
	}

	// Each unit ran as a governed child of the parent, narrowed to what the unit asked
	// for: the right to fan out again did not flow down a plan any more than it flows
	// down a tool call.
	children := childrenOf(ctx, t, store, "root")
	if len(children) != 2 {
		t.Fatalf("expected one child per unit, got %d", len(children))
	}
	for _, c := range children {
		cs, err := goal.DecodeSpec(c)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs.Grant) != 1 || cs.Grant[0] != mission.ActionModelGenerate {
			t.Fatalf("child grant = %v, want [%s]", cs.Grant, mission.ActionModelGenerate)
		}
		if len(cs.Ledger) != 1 {
			t.Fatalf("child %s did not carry its unit's ledger: %+v", c.Name, cs.Ledger)
		}
	}
}
