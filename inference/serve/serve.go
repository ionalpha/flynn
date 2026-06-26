// Package serve runs a local model server and keeps track of it. It is the step
// between a built launch plan (a serve command that has not been started) and a model a
// client can reach: it starts the runtime as a confined background process, waits for
// its loopback endpoint to answer, and records the running server so a later command
// can find, reuse, or stop it. Starting a server that is already up is a no-op, so the
// same call both starts and reuses.
//
// The runtime is the code-execution surface that parses untrusted weights, so it is
// started inside the sandbox rather than as a bare child. The package depends on small
// interfaces for launching, probing, and killing, so the whole lifecycle is testable
// with fakes and no live runtime, while the real wiring uses the sandbox and HTTP.
package serve

import (
	"context"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/inference/launch"
	"github.com/ionalpha/flynn/sandbox"
)

// Proc is the handle to a started background server the manager needs: enough to know
// whether it is up, read why it failed, wait for it, and stop it. *sandbox.Process
// satisfies it; a test supplies a fake.
type Proc interface {
	PID() int
	Running() bool
	Output() string
	Done() <-chan struct{}
	Stop() error
}

// Launcher starts a server process for a serve plan. The real launcher runs it inside
// the sandbox; a test launcher returns a fake Proc.
type Launcher interface {
	Serve(ctx context.Context, spec sandbox.ServeSpec) (Proc, error)
}

// Prober reports whether the model endpoint at baseURL is answering. It returns nil
// once the server is ready, and an error while it is not. The real prober makes a
// loopback HTTP request; a test prober scripts readiness.
type Prober func(ctx context.Context, baseURL string) error

// Killer stops a server identified only by its pid, the case where the server was
// started by an earlier, now-gone Flynn process and no Proc handle is held. The real
// killer signals the OS process; a test killer records the pid.
type Killer func(pid int) error

// Endpoint is a running local model server reachable over loopback.
type Endpoint struct {
	// ModelID is the catalog id being served.
	ModelID string
	// BaseURL is the OpenAI-compatible endpoint a client targets.
	BaseURL string
	// Port is the loopback port the server listens on.
	Port int
	// PID is the server process id.
	PID int
	// Reused is true when an already-running server was adopted instead of started.
	Reused bool
	// proc is the handle to a server this call started; nil when Reused, since the
	// process belongs to another invocation and is controlled through the registry.
	proc Proc
}

// Stop ends a server this call started. It is only meaningful for a freshly started,
// non-reused endpoint; for a reused one the owning process or `models stop` controls
// the lifecycle. It does not remove the registry record.
func (e Endpoint) Stop() error {
	if e.proc == nil {
		return nil
	}
	return e.proc.Stop()
}

// Manager starts, reuses, reports, and stops local model servers. Its dependencies are
// injected so the lifecycle is exercised without a live runtime.
type Manager struct {
	launcher Launcher
	probe    Prober
	kill     Killer
	reg      *Registry
	now      func() int64
	// readyTimeout caps how long Ensure waits for a started server to answer before
	// giving up and stopping it.
	readyTimeout time.Duration
	// pollEvery is how often Ensure probes the endpoint while waiting for readiness.
	pollEvery time.Duration
}

// Option configures a Manager.
type Option func(*Manager)

// WithReadyTimeout sets how long Ensure waits for a started server to become ready.
func WithReadyTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.readyTimeout = d
		}
	}
}

// WithPollInterval sets how often Ensure probes a starting server.
func WithPollInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.pollEvery = d
		}
	}
}

// withClock overrides the wall clock, for deterministic tests.
func withClock(now func() int64) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// NewManager builds a Manager from its launcher, prober, killer, and registry.
func NewManager(l Launcher, probe Prober, kill Killer, reg *Registry, opts ...Option) *Manager {
	m := &Manager{
		launcher:     l,
		probe:        probe,
		kill:         kill,
		reg:          reg,
		now:          func() int64 { return time.Now().Unix() },
		readyTimeout: 90 * time.Second,
		pollEvery:    250 * time.Millisecond,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// EnsureConfig is the input to Ensure: which model, the built serve plan, and how it is
// run.
type EnsureConfig struct {
	// ModelID is the catalog id, the registry key.
	ModelID string
	// Runtime names the runtime, recorded for display.
	Runtime string
	// Plan is the built, not-yet-started serve command and its loopback address.
	Plan launch.Plan
	// Confine requests the sandbox kernel confinement for the runtime process, which is
	// the right default since the runtime parses untrusted weights.
	Confine bool
}

// Ensure returns a running endpoint for the model, starting the server if one is not
// already up. If the registry already has a server for this model and its endpoint
// answers a health probe, that server is adopted (Reused). Otherwise a stale record is
// pruned, the server is started inside the sandbox from the plan, the manager waits for
// its endpoint to answer, records it, and returns it. A server that starts but never
// becomes ready, or that exits while starting, is stopped and reported as an error with
// its captured output, so a launch never leaves an orphan behind.
func (m *Manager) Ensure(ctx context.Context, cfg EnsureConfig) (Endpoint, error) {
	if cfg.ModelID == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_no_model", "serve: no model id")
	}
	if len(cfg.Plan.Argv) == 0 || cfg.Plan.BaseURL == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_bad_plan", "serve: plan is missing a command or endpoint")
	}

	// Reuse an already-running server when its endpoint actually answers.
	if rec, ok, err := m.reg.Get(cfg.ModelID); err != nil {
		return Endpoint{}, err
	} else if ok {
		if m.probe(ctx, rec.BaseURL) == nil {
			return Endpoint{ModelID: rec.ModelID, BaseURL: rec.BaseURL, Port: rec.Port, PID: rec.PID, Reused: true}, nil
		}
		// The record names a server that no longer answers; drop it before starting a
		// fresh one so the registry never points at a dead endpoint.
		if err := m.reg.Delete(cfg.ModelID); err != nil {
			return Endpoint{}, err
		}
	}

	proc, err := m.launcher.Serve(ctx, sandbox.ServeSpec{Argv: cfg.Plan.Argv, Confine: cfg.Confine})
	if err != nil {
		return Endpoint{}, fault.Wrap(fault.Terminal, "serve_start", err)
	}

	if err := m.waitReady(ctx, proc, cfg.Plan.BaseURL); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}

	rec := Record{
		ModelID:   cfg.ModelID,
		PID:       proc.PID(),
		Port:      cfg.Plan.Port,
		BaseURL:   cfg.Plan.BaseURL,
		Runtime:   cfg.Runtime,
		StartedAt: m.now(),
	}
	if err := m.reg.Put(rec); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}
	return Endpoint{ModelID: cfg.ModelID, BaseURL: cfg.Plan.BaseURL, Port: cfg.Plan.Port, PID: proc.PID(), proc: proc}, nil
}

// waitReady polls the endpoint until it answers, the process exits, the deadline
// passes, or ctx is cancelled. A process that exits while starting is the common
// real-world failure (a bad runtime flag, a refused weights file), so it is reported
// with the server's own output rather than as a bare timeout.
func (m *Manager) waitReady(ctx context.Context, proc Proc, baseURL string) error {
	deadline := time.NewTimer(m.readyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(m.pollEvery)
	defer tick.Stop()

	// Probe once immediately so an already-warm endpoint is not made to wait a tick.
	if m.probe(ctx, baseURL) == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proc.Done():
			return fault.New(fault.Terminal, "serve_exited",
				fmt.Sprintf("serve: the runtime exited before its endpoint came up:\n%s", proc.Output()))
		case <-deadline.C:
			return fault.New(fault.Terminal, "serve_timeout",
				fmt.Sprintf("serve: the runtime did not answer within %s:\n%s", m.readyTimeout, proc.Output()))
		case <-tick.C:
			if m.probe(ctx, baseURL) == nil {
				return nil
			}
		}
	}
}

// Status returns the recorded servers whose endpoint currently answers, pruning any
// record that no longer does so the report reflects reality rather than stale claims.
func (m *Manager) Status(ctx context.Context) ([]Record, error) {
	recs, err := m.reg.List()
	if err != nil {
		return nil, err
	}
	var live []Record
	for _, rec := range recs {
		if m.probe(ctx, rec.BaseURL) == nil {
			live = append(live, rec)
			continue
		}
		if err := m.reg.Delete(rec.ModelID); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// Stop ends the server for a model id and removes its record. It returns whether a
// server was found to stop. The process may belong to an earlier Flynn invocation, so
// it is stopped by pid through the killer rather than through a Proc handle.
func (m *Manager) Stop(modelID string) (bool, error) {
	rec, ok, err := m.reg.Get(modelID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if rec.PID > 0 {
		if err := m.kill(rec.PID); err != nil {
			return false, fault.Wrap(fault.Terminal, "serve_kill", err)
		}
	}
	if err := m.reg.Delete(modelID); err != nil {
		return false, err
	}
	return true, nil
}
