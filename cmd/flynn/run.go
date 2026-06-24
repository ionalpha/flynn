package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/tools"
)

// defaultSystemPrompt frames the agent for a coding/automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const defaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// runMission assembles the runtime over model and the sandboxed default toolset,
// opens a streaming session, submits objective as a goal, and renders the live
// event stream to out until the goal converges or stalls. The model's final
// summary is returned. The working directory is the sandbox root, so every command
// and file operation is confined to it.
func runMission(ctx context.Context, out io.Writer, model llm.Model, workdir, objective string) (string, error) {
	sb, err := sandbox.NewLocal(workdir)
	if err != nil {
		return "", err
	}
	w := &syncWriter{w: out}

	// The session records the conversation on an in-memory spine and fans it out
	// over an in-memory bus; the mission reporter feeds it every turn.
	sess := session.New(spine.NewMemoryLog(), bus.NewMemory())

	exec := mission.NewExecutor(
		model,
		mission.WithTools(tools.New(sb).Tools()...),
		mission.WithSystem(defaultSystemPrompt),
		mission.WithObserver(sess.Reporter()),
	)
	rt, err := runtime.New(runtime.Config{
		Executor:     exec,
		Stop:         mission.Convergence{},
		PollInterval: 200 * time.Millisecond,
		WorkerPoll:   50 * time.Millisecond,
	})
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = rt.Start(runCtx); close(done) }()

	events, err := sess.Subscribe(runCtx, 0)
	if err != nil {
		return "", err
	}
	if _, err := sess.Submit(runCtx, rt, goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	}); err != nil {
		return "", err
	}

	result, runErr := renderStream(w, events)
	cancel()
	<-done
	return result, runErr
}

// renderStream prints the session's events as they arrive and returns once the
// session reaches a terminal event: the model's summary on convergence, or an
// error on stall. A closed channel before any terminal event means the run was
// cancelled.
func renderStream(out io.Writer, events <-chan session.Event) (string, error) {
	for ev := range events {
		switch ev.Kind {
		case session.KindTurnStarted:
			_, _ = fmt.Fprintf(out, "  ... turn %d\n", ev.Turn)
		case session.KindToolCall:
			_, _ = fmt.Fprintf(out, "  -> %s\n", ev.Tool)
		case session.KindConverged:
			return ev.Text, nil
		case session.KindStalled:
			return "", fmt.Errorf("goal stalled: %s", ev.Err)
		}
	}
	return "", context.Canceled
}

// syncWriter serializes writes, so the stream-rendering goroutine and any other
// writer never interleave or race on the underlying writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
