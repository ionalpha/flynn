// Package agent is the Ion Alpha open-source agent runtime.
//
// It is transport- and provider-agnostic, builds to a single static binary
// (see cmd/flynn), and exposes importable packages so a host application can
// embed it directly. Persistence is reached only through the interfaces in the
// state package, and observability through the observe package: the open agent ships
// local implementations and no-op defaults, while a richer host (e.g. an Ion
// Alpha instance) can supply its own, without this package depending on the host.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/tools"
)

// DefaultSystemPrompt frames the agent for a coding or automation task. It is kept
// short on purpose: a capable model works better from a clear goal than from a long
// list of rules.
const DefaultSystemPrompt = `You are Flynn, an autonomous software agent working inside a sandboxed working directory.
You have tools to run shell commands and to read, write, edit, glob, and grep files; every command and file path is confined to the working directory.
Work toward the objective directly: inspect what you need, make the changes, and verify them with the tools rather than guessing.
When the objective is fully accomplished, stop and reply with a short summary of what you did.`

// Config configures an Agent. The zero value is usable: New fills in safe,
// standalone defaults so the agent always runs without external services.
type Config struct {
	// Model is the provider:model identifier (e.g. "anthropic:claude-opus-4-8").
	// Goal resolves it to a concrete model, reading the provider's API key from the
	// environment.
	Model string
	// Objective, when set, is the goal Run drives to completion. Programmatic
	// callers can pass the objective to Goal directly instead.
	Objective string
	// WorkDir is the sandbox root every tool is confined to. Defaults to the current
	// working directory.
	WorkDir string
	// State is the durable backend for sessions, skills, and memory. If nil, an
	// in-memory provider is used so the agent runs with zero setup.
	State state.Provider
	// Obs carries the logger and tracer. If nil, no-op defaults are used.
	Obs *observe.Observability
	// Out is where human-facing output is written. Defaults to io.Discard.
	Out io.Writer
}

// Agent is the core runtime.
type Agent struct {
	cfg Config
}

// New constructs an Agent, filling in standalone defaults for any zero fields.
func New(cfg Config) *Agent {
	if cfg.State == nil {
		cfg.State = state.NewMemory()
	}
	if cfg.Obs == nil {
		cfg.Obs = observe.Default()
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Agent{cfg: cfg}
}

// Run drives Config.Objective to completion and writes the model's final answer to
// Config.Out. It is the embedding entry point: a host sets a model and an objective
// and calls Run. With no objective configured it reports that nothing was given to
// do; programmatic callers that want a returned result call Goal instead.
func (a *Agent) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	objective := strings.TrimSpace(a.cfg.Objective)
	if objective == "" {
		return errors.New("agent: no objective set; set Config.Objective or call Goal")
	}
	result, err := a.Goal(ctx, objective)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(a.cfg.Out, result)
	return nil
}

// Goal drives objective to completion through the sandboxed toolset and returns the
// model's final answer. It resolves the model from Config.Model (reading the
// provider's API key from the environment), confines every tool to the working
// directory, and governs each model call and tool call through the dispatch waist.
func (a *Agent) Goal(ctx context.Context, objective string) (string, error) {
	model, err := provider.Resolve(a.cfg.Model)
	if err != nil {
		return "", err
	}
	return a.runGoal(ctx, model, objective)
}

// runGoal assembles the in-process runtime over model and drives one goal to a
// terminal result. The model is injected so the assembly is exercised end to end in
// tests with a scripted backend, without a network or an API key.
func (a *Agent) runGoal(ctx context.Context, model llm.Model, objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", errors.New("agent: empty objective")
	}

	workdir := a.cfg.WorkDir
	if workdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		workdir = cwd
	}
	// Secure by default: a model-authored command runs kernel-confined where the
	// platform enforces it (read-only host, syscall-filtered), and at the process-jail
	// floor elsewhere.
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		return "", err
	}

	// The session streams the run as an event spine; its Reporter is wired into the
	// executor so every turn, tool call, and result is recorded.
	sess := session.New(spine.NewMemoryLog(), bus.NewMemory())

	// The grant lists exactly what the run may do: each tool, plus the model call,
	// so every action is admitted against it and the grant stays the complete record
	// of what this run can reach.
	toolset := tools.New(sb).Tools()
	names := make([]string, 0, len(toolset)+1)
	for _, t := range toolset {
		names = append(names, t.Def().Name)
	}
	names = append(names, mission.ActionModelGenerate)

	exec := mission.NewExecutor(
		model,
		mission.WithTools(toolset...),
		mission.WithSystem(DefaultSystemPrompt),
		mission.WithObserver(sess.Reporter()),
		mission.WithGrant(capability.NewGrant(names...)),
	)
	// A nil Store makes the runtime build its in-process substrate (store, queue,
	// bus) over a registry holding the core and Goal kinds.
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

	if _, err := sess.Submit(runCtx, rt, goal.Spec{
		Objective:     objective,
		StopCondition: "the objective is fully accomplished",
	}); err != nil {
		return "", err
	}
	result, err := sess.Wait(runCtx)
	cancel()
	<-done
	return result, err
}
