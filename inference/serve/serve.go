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

	"github.com/ionalpha/flynn/clock"
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

// ContainerStopper stops a container-backed server by its engine and id, the container
// counterpart to Killer: a container has no host pid, so a separate Flynn process stops it
// by driving the engine. The real stopper runs the engine CLI; a test stopper records the
// call. A nil stopper means container-backed records cannot be stopped across invocations,
// so the production wiring always supplies one.
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
	// stopContainer stops a container-backed server across invocations. Nil until the
	// production wiring supplies one, since the standalone process path never needs it.
	stopContainer ContainerStopper
	// runContainer starts a container under the tier's guarantees. It defaults to
	// sandbox.RunContainer and is injected in tests so the container lifecycle is exercised
	// without a real engine.
	runContainer func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error)
	reg          *Registry
	clk          clock.Clock
	// stats reads how loaded a running server is, one source per runtime name. Empty by
	// default so the standalone path stays zero-setup and reports load as unknown.
	stats map[string]StatsSource
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

// WithContainerStopper sets how a container-backed server is stopped across invocations, so
// `models stop` and the scheduler's evict can tear down a vLLM container the same way they
// kill a process-backed server. Without one, a container record cannot be stopped by a later
// process, so the production wiring always supplies it.
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

// withContainerRunner overrides how a container is started, so the container lifecycle is
// tested without a real engine.
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

	rec := Record{
		ModelID:   cfg.ModelID,
		PID:       proc.PID(),
		Port:      cfg.Plan.Port,
		BaseURL:   cfg.Plan.BaseURL,
		Runtime:   cfg.Runtime,
		StartedAt: m.clk.Now().Unix(),
	}
	if err := m.reg.Put(rec); err != nil {
		_ = proc.Stop()
		return Endpoint{}, err
	}
	return Endpoint{ModelID: cfg.ModelID, BaseURL: cfg.Plan.BaseURL, Port: cfg.Plan.Port, PID: proc.PID(), proc: proc}, nil
}

// ContainerEnsureConfig is the input to EnsureContainer: which model, the validated
// container request, and the loopback endpoint to probe. The Spec's Command is the server
// invocation (the vLLM serve argv), built by the caller from the launch plan.
type ContainerEnsureConfig struct {
	// ModelID is the catalog id, the registry key.
	ModelID string
	// Runtime names the runtime, recorded for display (for example "vllm").
	Runtime string
	// Spec is the validated container request: the digest-pinned image, the untrusted
	// guarantees, the GPU grant, the published loopback port, and the server command.
	Spec sandbox.ContainerSpec
	// BaseURL and Port are the loopback endpoint the served model answers on, the same
	// OpenAI-compatible coordinates a process-backed server reports.
	BaseURL string
	Port    int
	// ReadyTimeout overrides how long to wait for the endpoint to come up. A container
	// runtime can be far slower to first answer than a process (a GPU server loads weights,
	// captures CUDA graphs, and compiles kernels on first start), so the caller raises it
	// above the manager's process-oriented default. Zero uses the manager default.
	ReadyTimeout time.Duration
}

// EnsureContainer returns a running endpoint for a container-backed model, the container
// counterpart to Ensure. It reuses an already-running server when its endpoint answers,
// otherwise runs the container under the tier's guarantees, waits for the endpoint to come
// up, records it with its container identity (so a later process can stop it), and returns
// it. A container that starts but never becomes ready, or exits while starting, is stopped
// and reported with its captured output, so a launch never leaks a container. It drives the
// same registry and reuse path as Ensure, so the scheduler and `models status`/`stop` see
// process- and container-backed servers uniformly: one supervisor, two runtime shapes.
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

// reuseRunning returns an endpoint for an already-running server when its recorded endpoint
// still answers a health probe, adopting it as Reused. A record whose server no longer
// answers is dropped before the caller starts a fresh one, so the registry never points at a
// dead endpoint. It is the shared reuse path for both the process and container launches.
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

// containerIdentified is the optional interface a container-backed Serving implements to
// report the engine and id a later process needs to stop it.
type containerIdentified interface {
	ContainerID() string
	EngineName() string
}

// servingProc adapts a sandbox.Serving to the Proc the manager's wait and endpoint use. A
// container has no host pid, so PID is 0; identity travels in the registry record instead.
type servingProc struct{ s sandbox.Serving }

func (p servingProc) PID() int              { return 0 }
func (p servingProc) Running() bool         { return p.s.Running() }
func (p servingProc) Output() string        { return p.s.Output() }
func (p servingProc) Done() <-chan struct{} { return p.s.Done() }
func (p servingProc) Stop() error           { return p.s.Stop() }

// waitable is the minimal view of a starting server waitReady needs: a signal for the
// server exiting and its captured output for a failure message. Both a process Proc and a
// container servingProc satisfy it.
type waitable interface {
	Done() <-chan struct{}
	Output() string
}

// waitReady polls the endpoint until it answers, the server exits, the deadline
// passes, or ctx is cancelled. A server that exits while starting is the common
// real-world failure (a bad runtime flag, a refused weights file), so it is reported
// with the server's own output rather than as a bare timeout.
func (m *Manager) waitReady(ctx context.Context, proc waitable, baseURL string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
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
				"serve: the runtime exited before its endpoint came up:\n"+proc.Output())
		case <-deadline.C:
			return fault.New(fault.Terminal, "serve_timeout",
				fmt.Sprintf("serve: the runtime did not answer within %s:\n%s", timeout, proc.Output()))
		case <-tick.C:
			if m.probe(ctx, baseURL) == nil {
				return nil
			}
		}
	}
}

// Status returns the recorded servers whose endpoint currently answers, pruning any
// record that no longer does so the report reflects reality rather than stale claims. A
// pruned server's process is reclaimed: a record is only written after the server became
// ready, so an endpoint that has stopped answering means the runtime died or wedged, and a
// wedged runtime left running would keep holding device memory after the manager has
// forgotten it. Killing it before dropping the record prevents that leak; killing an
// already-exited process is a no-op.
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
	if err := m.teardown(rec); err != nil {
		return false, fault.Wrap(fault.Terminal, "serve_stop", err)
	}
	if err := m.reg.Delete(modelID); err != nil {
		return false, err
	}
	return true, nil
}

// teardown stops the server a record names, routing to the engine for a container-backed
// record and to the OS for a process-backed one. A container record with no stopper wired, or
// a record with neither identity, is a no-op so a reclaim or stop never errors on a server it
// cannot signal.
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
