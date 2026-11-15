// Package serve runs a local model server and keeps track of it. It starts a built
// launch plan as a confined background process, waits for its loopback endpoint to
// answer, and records the server so a later command can find, reuse or stop it.
// Starting a server that is already up reuses it. The runtime parses untrusted
// weights, so it always starts inside the sandbox. Launching, probing and killing are
// small interfaces, so the lifecycle is testable without a live runtime.
package serve

import (
	"context"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/sandbox"
)

// Proc is the handle to a started background server. *sandbox.Process satisfies it.
type Proc interface {
	// PID is recorded so a later Flynn process can stop the server without this handle.
	// Zero when the process never started.
	PID() int
	// Running reports whether the process has not yet exited.
	Running() bool
	// Output is the retained tail of stdout and stderr, the only account of why a server
	// exited before its endpoint answered.
	Output() string
	// Done is closed when the process exits, so a readiness wait can select on it instead
	// of polling an endpoint that will never answer.
	Done() <-chan struct{}
	// Stop ends the process and waits for it. Stopping an exited process returns nil.
	Stop() error
}

// Launcher starts a server process for a serve plan, inside the sandbox in production.
type Launcher interface {
	Serve(ctx context.Context, spec sandbox.ServeSpec) (Proc, error)
}

// Prober returns nil once the model endpoint at baseURL answers, and an error until
// then.
type Prober func(ctx context.Context, baseURL string) error

// Killer stops a server by pid alone, for a server an earlier Flynn process started.
type Killer func(pid int) error

// ContainerStopper stops a container-backed server by engine and id, since a container
// has no host pid. Without one, a container record cannot be stopped by a later
// process, so the production wiring always supplies it.
type ContainerStopper func(engine, id string) error

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
	// proc is nil when Reused, because the process belongs to another invocation and is
	// controlled through the registry.
	proc Proc
}

// Stop ends a server this call started. A reused endpoint's lifecycle belongs to its
// owning process or `models stop`. It leaves the registry record in place.
func (e Endpoint) Stop() error {
	if e.proc == nil {
		return nil
	}
	return e.proc.Stop()
}

// Manager starts, reuses, reports, and stops local model servers.
type Manager struct {
	launcher Launcher
	probe    Prober
	kill     Killer
	// stopContainer stops a container-backed server across invocations. The standalone
	// process path leaves it nil.
	stopContainer ContainerStopper
	// runContainer starts a container under the tier's guarantees; it defaults to
	// sandbox.RunContainer.
	runContainer func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error)
	reg          *Registry
	clk          clock.Clock
	// stats holds one load reader per runtime name. Empty by default, so load reads as
	// unknown with no setup.
	stats map[string]StatsSource
	// readyTimeout caps how long Ensure waits for a started server before stopping it.
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

// WithContainerStopper sets how a container-backed server is stopped across
// invocations, so `models stop` and the scheduler's evict can tear down a vLLM
// container the way they kill a process-backed one.
func WithContainerStopper(s ContainerStopper) Option {
	return func(m *Manager) {
		if s != nil {
			m.stopContainer = s
		}
	}
}

// withClock overrides the time source, for deterministic tests.
func withClock(c clock.Clock) Option {
	return func(m *Manager) {
		if c != nil {
			m.clk = c
		}
	}
}

// withContainerRunner overrides how a container is started, so tests need no engine.
func withContainerRunner(run func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error)) Option {
	return func(m *Manager) {
		if run != nil {
			m.runContainer = run
		}
	}
}

// NewManager builds a Manager from its launcher, prober, killer, and registry.
func NewManager(l Launcher, probe Prober, kill Killer, reg *Registry, opts ...Option) *Manager {
	m := &Manager{
		launcher:     l,
		probe:        probe,
		kill:         kill,
		runContainer: sandbox.RunContainer,
		reg:          reg,
		clk:          clock.System{},
		stats:        map[string]StatsSource{},
		readyTimeout: 90 * time.Second,
		pollEvery:    250 * time.Millisecond,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// EnsureConfig is the input to Ensure.
type EnsureConfig struct {
	// ModelID is the catalog id, the registry key.
	ModelID string
	// Runtime names the runtime, recorded for display.
	Runtime string
	// Plan is the built, not-yet-started serve command and its loopback address.
	Plan launch.Plan
	// Confine requests kernel confinement, the right default since the runtime parses
	// untrusted weights.
	Confine bool
	// Version is the runtime build, recorded so a later process can hold the running
	// server to the advisory floor.
	Version string
	// Containment is the isolation the sandbox reported, recorded so a later process can
	// refuse to adopt a server that ran under less than it requires.
	Containment string
	// Attest runs after the endpoint answers and before the server is recorded, so the
	// caller can prove the listener is the process this call started; a probe sees
	// whoever holds the port. An error stops the server and fails the call. The server
	// must still be running afterwards, so a runtime that lost the bind race cannot pass
	// on the strength of the process that beat it.
	Attest func(ctx context.Context, baseURL string) error
}

// Ensure returns a running endpoint for the model. A recorded server whose endpoint
// answers is adopted (Reused); otherwise a stale record is pruned and the server is
// started in the sandbox, awaited, and recorded. A server that never becomes ready or
// exits while starting is stopped and reported with its output, so a launch never
// leaves an orphan.
func (m *Manager) Ensure(ctx context.Context, cfg EnsureConfig) (Endpoint, error) {
	if cfg.ModelID == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_no_model", "serve: no model id")
	}
	if len(cfg.Plan.Argv) == 0 || cfg.Plan.BaseURL == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_bad_plan", "serve: plan is missing a command or endpoint")
	}

	if ep, ok, err := m.reuseRunning(ctx, cfg.ModelID); err != nil {
		return Endpoint{}, err
	} else if ok {
		return ep, nil
	}

	proc, err := m.launcher.Serve(ctx, sandbox.ServeSpec{Argv: cfg.Plan.Argv, Confine: cfg.Confine})
	if err != nil {
		return Endpoint{}, fault.Wrap(fault.Terminal, "serve_start", err)
	}

	if err := m.waitReady(ctx, proc, cfg.Plan.BaseURL, m.readyTimeout); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}
	if err := m.attest(ctx, proc, cfg); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}

	rec := Record{
		ModelID:     cfg.ModelID,
		PID:         proc.PID(),
		Port:        cfg.Plan.Port,
		BaseURL:     cfg.Plan.BaseURL,
		Runtime:     cfg.Runtime,
		Version:     cfg.Version,
		Containment: cfg.Containment,
		StartedAt:   m.clk.Now().Unix(),
	}
	if err := m.reg.Put(rec); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}
	return Endpoint{ModelID: cfg.ModelID, BaseURL: cfg.Plan.BaseURL, Port: cfg.Plan.Port, PID: proc.PID(), proc: proc}, nil
}

// attest runs the caller's attestation, then confirms the started process is still
// running, so an answer from a squatter after the runtime exited is refused.
func (m *Manager) attest(ctx context.Context, proc Proc, cfg EnsureConfig) error {
	if cfg.Attest == nil {
		return nil
	}
	if err := cfg.Attest(ctx, cfg.Plan.BaseURL); err != nil {
		return err
	}
	select {
	case <-proc.Done():
		return fault.New(fault.Terminal, "serve_exited",
			"serve: the runtime exited while its endpoint was being attested, so something else holds the port:\n"+proc.Output())
	default:
		return nil
	}
}

// ContainerEnsureConfig is the input to EnsureContainer. The Spec's Command is the
// server argv the caller built from the launch plan.
type ContainerEnsureConfig struct {
	// ModelID is the catalog id, the registry key.
	ModelID string
	// Runtime names the runtime, recorded for display (for example "vllm").
	Runtime string
	// Spec is the validated container request: the pinned image, the untrusted
	// guarantees, the GPU grant, the loopback port and the server command.
	Spec sandbox.ContainerSpec
	// BaseURL and Port are the loopback endpoint the model answers on.
	BaseURL string
	Port    int
	// ReadyTimeout overrides the wait for the endpoint, because a GPU server loads
	// weights, captures CUDA graphs and compiles kernels on first start. Zero uses the
	// manager default.
	ReadyTimeout time.Duration
}

// EnsureContainer is the container counterpart to Ensure. It records the container's
// identity so a later process can stop it, and stops a container that never becomes
// ready. It shares Ensure's registry and reuse path, so the scheduler and
// `models status` and `models stop` treat both server shapes alike.
func (m *Manager) EnsureContainer(ctx context.Context, cfg ContainerEnsureConfig) (Endpoint, error) {
	if cfg.ModelID == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_no_model", "serve: no model id")
	}
	if cfg.BaseURL == "" {
		return Endpoint{}, fault.New(fault.Terminal, "serve_bad_plan", "serve: container plan is missing an endpoint")
	}

	if ep, ok, err := m.reuseRunning(ctx, cfg.ModelID); err != nil {
		return Endpoint{}, err
	} else if ok {
		return ep, nil
	}

	serving, err := m.runContainer(ctx, cfg.Spec)
	if err != nil {
		return Endpoint{}, fault.Wrap(fault.Terminal, "serve_container_start", err)
	}
	proc := servingProc{serving}

	timeout := m.readyTimeout
	if cfg.ReadyTimeout > 0 {
		timeout = cfg.ReadyTimeout
	}
	if err := m.waitReady(ctx, proc, cfg.BaseURL, timeout); err != nil {
		_ = serving.Stop()
		return Endpoint{}, err
	}

	rec := Record{
		ModelID:   cfg.ModelID,
		Port:      cfg.Port,
		BaseURL:   cfg.BaseURL,
		Runtime:   cfg.Runtime,
		StartedAt: m.clk.Now().Unix(),
	}
	if ci, ok := serving.(containerIdentified); ok {
		rec.ContainerID = ci.ContainerID()
		rec.Engine = ci.EngineName()
	}
	if err := m.reg.Put(rec); err != nil {
		_ = serving.Stop()
		return Endpoint{}, err
	}
	return Endpoint{ModelID: cfg.ModelID, BaseURL: cfg.BaseURL, Port: cfg.Port, proc: proc}, nil
}

// reuseRunning adopts a recorded server whose endpoint still answers. A record whose
// server no longer answers is dropped, so the registry never points at a dead endpoint.
func (m *Manager) reuseRunning(ctx context.Context, modelID string) (Endpoint, bool, error) {
	rec, ok, err := m.reg.Get(modelID)
	if err != nil || !ok {
		return Endpoint{}, false, err
	}
	if m.probe(ctx, rec.BaseURL) == nil {
		return Endpoint{ModelID: rec.ModelID, BaseURL: rec.BaseURL, Port: rec.Port, PID: rec.PID, Reused: true}, true, nil
	}
	if err := m.reg.Delete(modelID); err != nil {
		return Endpoint{}, false, err
	}
	return Endpoint{}, false, nil
}

// containerIdentified is implemented by a container-backed Serving to report the
// engine and id a later process needs to stop it.
type containerIdentified interface {
	ContainerID() string
	EngineName() string
}

// servingProc adapts a sandbox.Serving to [Proc]. A container has no host pid, so PID
// is 0 and the identity travels in the registry record.
type servingProc struct{ s sandbox.Serving }

func (p servingProc) PID() int              { return 0 }
func (p servingProc) Running() bool         { return p.s.Running() }
func (p servingProc) Output() string        { return p.s.Output() }
func (p servingProc) Done() <-chan struct{} { return p.s.Done() }
func (p servingProc) Stop() error           { return p.s.Stop() }

// waitable is the part of a starting server waitReady needs: its exit signal and its
// captured output.
type waitable interface {
	Done() <-chan struct{}
	Output() string
}

// waitReady polls the endpoint until it answers, the server exits, the deadline passes,
// or ctx is cancelled. An exit while starting is the common failure (a bad flag, a
// refused weights file), so it is reported with the server's output.
//
// An answer counts only while the started process is still running, but the exit can
// come after the check, so an answer only proves that something is listening.
// [EnsureConfig.Attest], with a credential sent to the listener, decides whether it is
// this call's server.
func (m *Manager) waitReady(ctx context.Context, proc waitable, baseURL string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(m.pollEvery)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proc.Done():
			return fault.New(fault.Terminal, "serve_exited",
				"serve: the runtime exited before its endpoint came up:\n"+proc.Output())
		case <-deadline.C:
			return fault.New(fault.Terminal, "serve_timeout",
				fmt.Sprintf("serve: the runtime did not answer within %s:\n%s", timeout, proc.Output()))
		case <-tick.C:
			if m.probe(ctx, baseURL) != nil {
				continue
			}
			select {
			case <-proc.Done():
				return fault.New(fault.Terminal, "serve_exited",
					"serve: the endpoint answered but the runtime had exited, so something else holds the port:\n"+proc.Output())
			default:
				return nil
			}
		}
	}
}

// Status returns the recorded servers whose endpoint answers and prunes the rest. A
// record is written only after the server was ready, so a silent endpoint means a dead
// or wedged runtime; its process is killed before the record is dropped, so a wedged
// runtime cannot keep holding device memory.
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
		_ = m.teardown(rec) // best-effort reclaim; an already-gone server is a no-op
		if err := m.reg.Delete(rec.ModelID); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// Stop ends the server for a model id and removes its record, reporting whether one was
// found. The process may belong to an earlier invocation, so it is stopped by pid.
func (m *Manager) Stop(modelID string) (bool, error) {
	rec, ok, err := m.reg.Get(modelID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := m.teardown(rec); err != nil {
		return false, fault.Wrap(fault.Terminal, "serve_stop", err)
	}
	if err := m.reg.Delete(modelID); err != nil {
		return false, err
	}
	return true, nil
}

// teardown stops the server a record names, through the engine for a container record
// and the OS for a process record. A record it cannot signal is a no-op, so a reclaim
// or stop never errors on it.
func (m *Manager) teardown(rec Record) error {
	if rec.ContainerID != "" {
		if m.stopContainer == nil {
			return nil
		}
		return m.stopContainer(rec.Engine, rec.ContainerID)
	}
	if rec.PID > 0 {
		return m.kill(rec.PID)
	}
	return nil
}
