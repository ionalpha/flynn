// Package agent is the Ion Alpha open-source agent runtime.
//
// It is transport- and provider-agnostic, builds to a single static binary
// (see cmd/flynn), and exposes importable packages so a host application can
// embed it directly. Persistence and context are reached only through the
// interfaces in the state package: the open agent ships local implementations,
// while a richer host (e.g. an Ion Alpha instance) can supply its own,
// without this package ever depending on the host.
package agent

import (
	"context"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/state"
)

// Config configures an Agent. The zero value is usable: New fills in safe,
// standalone defaults so the agent always runs without external services.
type Config struct {
	// Model is the provider:model identifier (e.g. "anthropic:claude-opus-4-8").
	Model string
	// Store is the durable backend for sessions, skills, and memory.
	// If nil, an in-memory store is used so the agent runs with zero setup.
	Store state.Store
	// Out is where human-facing output is written. Defaults to io.Discard.
	Out io.Writer
}

// Agent is the core runtime.
type Agent struct {
	cfg Config
}

// New constructs an Agent, filling in standalone defaults for any zero fields.
func New(cfg Config) *Agent {
	if cfg.Store == nil {
		cfg.Store = state.NewMemory()
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
	_, _ = fmt.Fprintf(a.cfg.Out, "flynn: runtime ready (model=%q, store=%s)\n", a.cfg.Model, a.cfg.Store.Name())
	return nil
}
