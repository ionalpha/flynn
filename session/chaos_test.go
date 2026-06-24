package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// assertWellFormed checks the structural invariants every completed session
// stream must hold: it opens with session.started, ends with a terminal event,
// carries a dense Seq from 1, never lets a turn index run backwards, and pairs
// every tool result with an earlier tool call. A single helper makes the
// invariant the same across the end-to-end and chaos tests.
func assertWellFormed(t *testing.T, evs []Event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("empty stream")
	}
	if evs[0].Kind != KindSessionStarted {
		t.Fatalf("first event = %q, want %q", evs[0].Kind, KindSessionStarted)
	}
	if last := evs[len(evs)-1].Kind; last != KindConverged && last != KindStalled {
		t.Fatalf("last event = %q, want a terminal event", last)
	}
	open := map[string]bool{}
	lastTurn := 0
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (stream not dense)", i, e.Seq, i+1)
		}
		if e.Turn > 0 { // conversation events; session-level events carry turn 0
			if e.Turn < lastTurn {
				t.Fatalf("turn went backwards: %d after %d", e.Turn, lastTurn)
			}
			lastTurn = e.Turn
		}
		switch e.Kind {
		case KindToolCall:
			open[e.ToolUseID] = true
		case KindToolResult:
			if !open[e.ToolUseID] {
				t.Fatalf("tool result %q has no preceding call", e.ToolUseID)
			}
		}
	}
}

// TestSessionStreamSurvivesFlakyExecutor injects a run of step failures with the
// deterministic testkit fault plan and shows the session still streams a coherent
// conversation to convergence: retried steps make no spurious emissions, the
// stream stays well formed, and Wait returns the final answer.
func TestSessionStreamSurvivesFlakyExecutor(t *testing.T) {
	for _, failures := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("failures_%d", failures), func(t *testing.T) {
			model := llmtest.NewScripted(
				llmtest.CallTool("c1", "echo", []byte(`{"v":1}`)),
				llmtest.SayText("done"),
			)
			sess := New(spine.NewMemoryLog(), bus.NewMemory(), WithPollInterval(5*time.Millisecond))
			inner := mission.NewExecutor(model, mission.WithTools(echoTool()), mission.WithObserver(sess.Reporter()))
			flaky := testkit.FaultyExecutor(inner, testkit.FailFirst(failures, fault.New(fault.Transient, "flaky", "retry")))

			rt, err := runtime.New(runtime.Config{
				Executor:           flaky,
				Stop:               mission.Convergence{},
				PollInterval:       15 * time.Millisecond,
				WorkerPoll:         5 * time.Millisecond,
				WorkerRetryBase:    2 * time.Millisecond,
				WorkerRetryCeiling: 10 * time.Millisecond,
				StepMaxAttempts:    50, // comfortably above the injected failure run
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			go func() { _ = rt.Start(ctx) }()

			ch, err := sess.Subscribe(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sess.Submit(ctx, rt, goal.Spec{Objective: "do it", StopCondition: "done"}); err != nil {
				t.Fatal(err)
			}

			evs := collectUntil(t, ch, KindConverged)
			assertWellFormed(t, evs)
			result, err := sess.Wait(ctx)
			if err != nil || result != "done" {
				t.Fatalf("Wait = (%q, %v), want (\"done\", nil)", result, err)
			}
		})
	}
}

// TestSessionStreamDeliversWithDeadBus proves the bus is a pure optimization: with
// a bus that never delivers a wake, the poll floor still drives the subscriber
// through the whole conversation. This is the level-triggered guarantee that keeps
// the stream correct when the live signal path is degraded or absent.
func TestSessionStreamDeliversWithDeadBus(t *testing.T) {
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "echo", []byte(`{}`)),
		llmtest.SayText("finished"),
	)
	// A dead bus (no inner): Subscribe yields an inert subscription, Publish is a
	// no-op. Only the stream poll floor can advance the subscriber.
	sess := New(spine.NewMemoryLog(), testkit.FaultyBus(nil, nil),
		WithPollInterval(5*time.Millisecond), WithStreamPoll(5*time.Millisecond))
	exec := mission.NewExecutor(model, mission.WithTools(echoTool()), mission.WithObserver(sess.Reporter()))
	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		PollInterval: 15 * time.Millisecond,
		WorkerPoll:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = rt.Start(ctx) }()

	ch, err := sess.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(ctx, rt, goal.Spec{Objective: "do it", StopCondition: "done"}); err != nil {
		t.Fatal(err)
	}

	evs := collectUntil(t, ch, KindConverged)
	assertWellFormed(t, evs)
	if evs[len(evs)-1].Text != "finished" {
		t.Fatalf("converged text = %q", evs[len(evs)-1].Text)
	}
}

// TestStreamToleratesFailingPublish shows a broken signal bus never breaks an
// append: with every Publish failing, the durable record still lands and the poll
// floor delivers it. append must report success because the spine write, not the
// wake, is what subscribers read.
func TestStreamToleratesFailingPublish(t *testing.T) {
	faultyBus := testkit.FaultyBus(bus.NewMemory(), testkit.Always(errors.New("bus down")))
	s := newStream(spine.NewMemoryLog(), faultyBus, "pub")
	s.poll = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := s.append(ctx, Event{Kind: KindTurnStarted, Turn: i + 1}); err != nil {
			t.Fatalf("append %d returned error despite only the publish failing: %v", i, err)
		}
	}
	assertSeqs(t, collect(t, ch, 4), 1, 2, 3, 4)
}

// TestStreamToleratesFlakyLog shows a flaky durable log degrades without
// corrupting the stream: appends that fail record nothing and assign no Seq, so
// the events that do land remain a dense, gap-free 1..N. A dropped event simply
// never existed rather than leaving a hole a subscriber would stall on.
func TestStreamToleratesFlakyLog(t *testing.T) {
	// Fail every second append; the survivors must still form a contiguous stream.
	flakyLog := testkit.FaultyLog(spine.NewMemoryLog(), testkit.FailEvery(2, errors.New("log io")))
	s := newStream(flakyLog, bus.NewMemory(), "log")
	s.poll = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	const tries = 10
	landed := 0
	for i := range tries {
		if err := s.append(ctx, Event{Kind: KindTurnStarted, Turn: i + 1}); err == nil {
			landed++
		}
	}
	if landed == 0 || landed == tries {
		t.Fatalf("fault plan did not produce a partial outcome: %d/%d landed", landed, tries)
	}
	got := collect(t, ch, landed)
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("survivor %d seq = %d, want %d (log lost contiguity)", i, e.Seq, i+1)
		}
	}
}
