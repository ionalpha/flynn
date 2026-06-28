package driver_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// recordingModel captures the requests it receives and answers with a fixed text,
// so a test can see which model ran and whether tools were offered.
type recordingModel struct {
	text string
	mu   sync.Mutex
	reqs []llm.Request
}

func (m *recordingModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	return llmtest.SayText(m.text), nil
}

func (m *recordingModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

func echoTool() mission.Tool {
	return mission.Func(
		llm.Tool{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	)
}

func baseSpec() driver.Spec {
	return driver.Spec{
		Tools:    []mission.Tool{echoTool()},
		System:   "sys",
		Grant:    capability.NewGrant("echo", mission.ActionModelGenerate),
		HasGrant: true,
	}
}

// driveRouter steps a goal carrying spec through the router to convergence, returning
// the convergence reason.
func driveRouter(t *testing.T, r *driver.Router, spec goal.Spec, maxSteps int) string {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var prev json.RawMessage
	for range maxSteps {
		st := goal.Status{Checkpoint: prev}
		enc, err := st.Encode()
		if err != nil {
			t.Fatal(err)
		}
		res := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw, Status: enc}
		next, err := r.Execute(context.Background(), res)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		prev = next
		met, reason, err := r.Met(context.Background(), spec, goal.Status{Checkpoint: next})
		if err != nil {
			t.Fatal(err)
		}
		if met {
			return reason
		}
	}
	t.Fatal("did not converge")
	return ""
}

func TestRouterDefaultLoopOffersTools(t *testing.T) {
	dm := &recordingModel{text: "default answer"}
	r := driver.NewRouter(driver.RouterConfig{Base: baseSpec(), DefaultModel: dm})
	driveRouter(t, r, goal.Spec{Objective: "o", StopCondition: "done"}, 5)
	if dm.calls() == 0 || len(dm.reqs[0].Tools) == 0 {
		t.Fatalf("default loop should offer tools to the model, got %d tools", len(dm.reqs[0].Tools))
	}
}

func TestRouterSingleShotLoopOffersNoTools(t *testing.T) {
	dm := &recordingModel{text: "single answer"}
	r := driver.NewRouter(driver.RouterConfig{Base: baseSpec(), DefaultModel: dm})
	reason := driveRouter(t, r, goal.Spec{Objective: "o", StopCondition: "done", Driver: driver.NameSingleShot}, 5)
	if dm.calls() != 1 {
		t.Fatalf("single-shot should call the model once, got %d", dm.calls())
	}
	if len(dm.reqs[0].Tools) != 0 {
		t.Fatalf("single-shot must not offer tools, got %d", len(dm.reqs[0].Tools))
	}
	if reason != "single answer" {
		t.Fatalf("reason = %q, want the model's single-shot answer", reason)
	}
}

func TestRouterResolvesPerGoalModel(t *testing.T) {
	def := &recordingModel{text: "default model"}
	m2 := &recordingModel{text: "model two"}
	var resolved []string
	r := driver.NewRouter(driver.RouterConfig{
		Base:         baseSpec(),
		DefaultModel: def,
		ResolveModel: func(id string) (llm.Model, error) {
			resolved = append(resolved, id)
			if id == "m2" {
				return m2, nil
			}
			return nil, fault.New(fault.Terminal, "unknown_model", "unknown model "+id)
		},
	})
	reason := driveRouter(t, r, goal.Spec{Objective: "o", StopCondition: "done", Driver: driver.NameSingleShot, Model: "m2"}, 5)
	if reason != "model two" {
		t.Fatalf("reason = %q, want m2's answer (per-goal model resolved)", reason)
	}
	if def.calls() != 0 {
		t.Fatal("the default model must not run a goal that names another model")
	}
	if len(resolved) != 1 || resolved[0] != "m2" {
		t.Fatalf("resolver calls = %v, want [m2]", resolved)
	}
}

func TestRouterCachesLoopPerDriverModel(t *testing.T) {
	def := &recordingModel{text: "x"}
	m2 := &recordingModel{text: "y"}
	calls := 0
	r := driver.NewRouter(driver.RouterConfig{
		Base:         baseSpec(),
		DefaultModel: def,
		ResolveModel: func(string) (llm.Model, error) { calls++; return m2, nil },
	})
	spec := goal.Spec{Objective: "o", StopCondition: "done", Driver: driver.NameSingleShot, Model: "m2"}
	driveRouter(t, r, spec, 5)
	driveRouter(t, r, spec, 5)
	if calls != 1 {
		t.Fatalf("model resolved %d times, want 1 (loop cached per driver+model)", calls)
	}
}

func TestRouterUnknownDriverFailsClosed(t *testing.T) {
	r := driver.NewRouter(driver.RouterConfig{Base: baseSpec(), DefaultModel: &recordingModel{text: "x"}})
	raw, _ := json.Marshal(goal.Spec{Objective: "o", StopCondition: "done", Driver: "no-such-driver"})
	_, err := r.Execute(context.Background(), resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw})
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("an unknown driver must fail closed, got: %v", err)
	}
}

func TestRouterModelNamedWithoutResolverErrors(t *testing.T) {
	r := driver.NewRouter(driver.RouterConfig{Base: baseSpec(), DefaultModel: &recordingModel{text: "x"}})
	raw, _ := json.Marshal(goal.Spec{Objective: "o", StopCondition: "done", Model: "m2"})
	_, err := r.Execute(context.Background(), resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw})
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("naming a model with no resolver must error, got: %v", err)
	}
}
