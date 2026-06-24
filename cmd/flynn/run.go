package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
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
// submits objective as a goal, and drives it to completion. Progress is written to
// out and the model's final summary is returned. The working directory is the
// sandbox root, so every command and file operation is confined to it.
func runMission(ctx context.Context, out io.Writer, model llm.Model, workdir, objective string) (string, error) {
	sb, err := sandbox.NewLocal(workdir)
	if err != nil {
		return "", err
	}
	w := &syncWriter{w: out}

	exec := mission.NewExecutor(
		model,
		mission.WithTools(tools.New(sb).Tools()...),
		mission.WithSystem(defaultSystemPrompt),
		mission.WithEventSink(actionPrinter{w}),
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

	g, err := rt.SubmitGoal(runCtx, "", goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	})
	if err != nil {
		return "", err
	}
	result, err := waitForResult(runCtx, w, rt, g.Key())
	cancel()
	<-done
	return result, err
}

// waitForResult polls the goal until it reaches a terminal phase, printing each new
// step, and returns the model's final summary on convergence or an error on stall.
func waitForResult(ctx context.Context, out io.Writer, rt *runtime.Runtime, key resource.Key) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	last := -1
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			r, err := rt.Store().Get(context.Background(), key.Kind, key.Scope, key.Name)
			if err != nil {
				continue
			}
			st, err := goal.DecodeStatus(r)
			if err != nil {
				continue
			}
			if st.Steps != last {
				last = st.Steps
				if st.Steps > 0 {
					_, _ = fmt.Fprintf(out, "  ... step %d\n", st.Steps)
				}
			}
			switch st.Phase {
			case goal.PhaseConverged:
				return st.Message, nil
			case goal.PhaseStalled:
				return "", fmt.Errorf("goal stalled: %s", st.Message)
			}
		}
	}
}

// actionPrinter writes a line per tool action as it starts, so a run shows its work.
type actionPrinter struct{ out io.Writer }

func (p actionPrinter) Append(_ context.Context, e dispatch.Event) error {
	if e.Type == dispatch.EventStart {
		_, _ = fmt.Fprintf(p.out, "  -> %s\n", e.Action)
	}
	return nil
}

// syncWriter serializes writes, so the worker goroutine's tool lines and the main
// goroutine's step lines never interleave or race on the underlying writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
