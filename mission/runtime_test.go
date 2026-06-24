package mission_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
)

// TestRuntimeDrivesMissionToConvergence is the full-stack proof of B: the assembled
// runtime takes a submitted Goal and drives it to Converged through the real
// control loop, where each step is a turn of a tool-using conversation with a
// (scripted) model. Nothing in the runtime, reconciler, or worker knows a language
// model is behind the executor.
func TestRuntimeDrivesMissionToConvergence(t *testing.T) {
	echo := mission.Func(
		llm.Tool{Name: "echo", Description: "echo input", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, input json.RawMessage) (string, error) { return string(input), nil },
	)
	model := llmtest.NewScripted(
		llmtest.CallTool("t1", "echo", json.RawMessage(`{"ping":true}`)),
		llmtest.SayText("mission complete"),
	)
	exec := mission.NewExecutor(model, mission.WithTools(echo), mission.WithSystem("you are an agent"))

	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		PollInterval: 15 * time.Millisecond,
		WorkerPoll:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(ctx); close(done) }()

	g, err := rt.SubmitGoal(ctx, "ship", goal.Spec{Objective: "ship the feature", StopCondition: "shipped"})
	if err != nil {
		t.Fatal(err)
	}

	st := waitForPhase(t, rt, g.Key(), goal.PhaseConverged, 5*time.Second)
	if st.Steps != 2 {
		t.Fatalf("converged in %d steps, want 2 (tool turn + final turn)", st.Steps)
	}
	if !strings.Contains(st.Message, "mission complete") {
		t.Fatalf("converged message did not carry the model's answer: %q", st.Message)
	}

	cancel()
	<-done
}

func waitForPhase(t *testing.T, rt *runtime.Runtime, key resource.Key, want goal.Phase, timeout time.Duration) goal.Status {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			st := readStatus(t, rt, key)
			t.Fatalf("goal did not reach %s in time; phase=%s steps=%d", want, st.Phase, st.Steps)
			return st
		case <-tick.C:
			if st := readStatus(t, rt, key); st.Phase == want {
				return st
			}
		}
	}
}

func readStatus(t *testing.T, rt *runtime.Runtime, key resource.Key) goal.Status {
	t.Helper()
	r, err := rt.Store().Get(context.Background(), key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatalf("get goal: %v", err)
	}
	st, err := goal.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}
