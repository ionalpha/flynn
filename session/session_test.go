package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// echoTool is a trivial mission tool that returns its input, enough to exercise a
// real tool call through the runtime without a sandbox.
func echoTool() mission.Tool {
	return mission.Func(
		llm.Tool{Name: "echo", Description: "echo input", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, in json.RawMessage) (string, error) { return string(in), nil },
	)
}

// newSessionRuntime assembles a session wired into a real goal runtime over a
// scripted model, the same composition cmd/flynn performs. It returns the session
// and a started runtime; the caller submits a goal and drives the stream.
func newSessionRuntime(t *testing.T, model llm.Model) (*Session, *runtime.Runtime, context.Context, context.CancelFunc) {
	t.Helper()
	sess := New(spine.NewMemoryLog(), bus.NewMemory(), WithPollInterval(5*time.Millisecond))
	exec := mission.NewExecutor(model, mission.WithTools(echoTool()), mission.WithObserver(sess.Reporter()))
	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		PollInterval: 20 * time.Millisecond,
		WorkerPoll:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	go func() { _ = rt.Start(ctx) }()
	return sess, rt, ctx, cancel
}

// collectUntil reads events until one of kind stop arrives (inclusive) or the
// timeout fires.
func collectUntil(t *testing.T, ch <-chan Event, stop Kind) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %q; got %v", stop, kindsOf(out))
			}
			out = append(out, ev)
			if ev.Kind == stop {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out before %q; got %v", stop, kindsOf(out))
		}
	}
}

func kindsOf(evs []Event) []Kind {
	out := make([]Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// TestSessionStreamsConvergedConversation is the end-to-end proof: a session wired
// into a real runtime streams the whole conversational arc of a goal that calls a
// tool then answers, ending in convergence, and Wait returns the final answer.
func TestSessionStreamsConvergedConversation(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{"v":1}`)),
		llmtest.SayText("all done here"),
	)
	sess, rt, ctx, cancel := newSessionRuntime(t, model)
	defer cancel()

	ch, err := sess.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(ctx, rt, goal.Spec{Objective: "do it", StopCondition: "done"}); err != nil {
		t.Fatal(err)
	}

	evs := collectUntil(t, ch, KindConverged)
	want := []Kind{
		KindSessionStarted,
		KindTurnStarted, KindToolCall, KindToolResult, KindTurnCompleted,
		KindTurnStarted, KindAssistant, KindTurnCompleted,
		KindConverged,
	}
	got := kindsOf(evs)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	assertWellFormed(t, evs)

	// Seqs are dense and monotonic, the contract a UI relies on to order and resume.
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
	}
	// The opening event carries the objective; the terminal one the final answer.
	if evs[0].Text != "do it" {
		t.Fatalf("started text = %q", evs[0].Text)
	}
	if evs[len(evs)-1].Text != "all done here" {
		t.Fatalf("converged text = %q", evs[len(evs)-1].Text)
	}

	result, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result != "all done here" {
		t.Fatalf("Wait result = %q", result)
	}
}

// TestSessionEmitsStalled confirms the terminal failure path: a goal whose step
// budget is exhausted before it converges ends the stream with a stalled event and
// a non-nil Wait error.
func TestSessionEmitsStalled(t *testing.T) {
	// One tool-call turn, no closing answer, with a one-step budget: the goal spends
	// its budget without converging and stalls.
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", json.RawMessage(`{}`)),
		llmtest.SayText("never reached"),
	)
	sess, rt, ctx, cancel := newSessionRuntime(t, model)
	defer cancel()

	ch, err := sess.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(ctx, rt, goal.Spec{Objective: "do it", StopCondition: "done", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}

	evs := collectUntil(t, ch, KindStalled)
	last := evs[len(evs)-1]
	if last.Kind != KindStalled || last.Err == "" {
		t.Fatalf("terminal event = %+v", last)
	}
	if _, err := sess.Wait(ctx); err == nil {
		t.Fatal("Wait returned nil error for a stalled goal")
	}
}

// TestSubmitNamesGoalAfterSession locks the identity-unification invariant: the
// goal a session submits is named after the session id, so a single id addresses
// both the run's event stream and its goal resource. A refactor that re-separates
// the two fails here.
func TestSubmitNamesGoalAfterSession(t *testing.T) {
	sess, rt, ctx, cancel := newSessionRuntime(t, llmtest.NewScripted(llmtest.SayText("ok")))
	defer cancel()

	key, err := sess.Submit(ctx, rt, goal.Spec{Objective: "do it", StopCondition: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != sess.ID() {
		t.Fatalf("goal name = %q, want it to match the session id %q", key.Name, sess.ID())
	}
	if sess.ID() == "" {
		t.Fatal("session id is empty; the default must be a generated id")
	}
}
