package mission_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
)

// TestRuntimeDrivesMissionToConvergence is the full-stack proof that the assembled
// runtime takes a submitted Goal and drives it to Converged through the real control
// loop, where each step is a turn of a tool-using conversation with a (scripted) model.
// Nothing in the runtime, reconciler, or worker knows a language model is behind the
// executor. It asserts the two turns ran by their durable effects (the tool executed,
// the final answer landed), not by the reconciler's observed step count, which is a
// lower bound under concurrent reconciliation.
func TestRuntimeDrivesMissionToConvergence(t *testing.T) {
	var echoCalls atomic.Int64
	echo := mission.Func(
		llm.Tool{Name: "echo", Description: "echo input", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, input json.RawMessage) (string, error) {
			echoCalls.Add(1)
			return string(input), nil
		},
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
	// The run took two turns: the tool turn, then the final answer. That both ran is
	// proven durably, by their effects, not by the observed step count. status.Steps is
	// a nondeterministic lower bound: the reconciler counts a step only when it observes
	// that step's job as done, but the goal can converge from the durable checkpoint on a
	// pass that never observes the job (a waiting step is excluded from the count, and a
	// job record already reaped is cleared without counting), so the observed count can
	// be anywhere from 0 up to the turns taken. Asserting any floor on it is a race. So
	// assert the tool actually ran and the final answer landed; those are deterministic.
	if got := echoCalls.Load(); got != 1 {
		t.Fatalf("tool turn: echo called %d times, want 1", got)
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
