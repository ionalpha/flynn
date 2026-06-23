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
	"fmt"
	"io"
	"log/slog"

	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/state"
)

// Config configures an Agent. The zero value is usable: New fills in safe,
// standalone defaults so the agent always runs without external services.
type Config struct {
	// Model is the provider:model identifier (e.g. "anthropic:claude-opus-4-8").
	Model string
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

// Run starts the agent. This is a skeleton entry point; the conversation loop,
// tool dispatch, router, and learning loop are wired in follow-up tasks.
func (a *Agent) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, span := a.cfg.Obs.Tracer.Start(ctx, "agent.Run")
	defer span.End()

	a.cfg.Obs.Log.InfoContext(ctx, "runtime ready",
		slog.String("model", a.cfg.Model),
		slog.String("store", a.cfg.State.Name()))
	_, _ = fmt.Fprintf(a.cfg.Out, "flynn: runtime ready (model=%q, store=%s)\n", a.cfg.Model, a.cfg.State.Name())
	return nil
}
