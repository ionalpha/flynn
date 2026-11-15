package playbook

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/sandbox"
)

// Result is the outcome of a playbook run.
type Result struct {
	Output  any              // the flow's return value
	Service *service.Service // nil when none was registered
}

// Runner runs a playbook's flow. The flow can do only what the wired ports allow:
// run a sandboxed command, resolve a dependency, confirm, stage a secret.
type Runner struct {
	exec     flow.Execer
	deps     flow.DependencyResolver
	confirm  flow.Confirmer
	sink     flow.CredentialSink
	observer flow.Observer
	svc      *service.Store
	clk      clock.Timing
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithClock sets the clock that stamps a registered service (default clock.System).
func WithClock(c clock.Timing) RunnerOption {
	return func(r *Runner) {
		if c != nil {
			r.clk = c
		}
	}
}

// WithConfirmer sets the port that asks the operator to approve confirm steps.
// Without it, a confirm step fails closed.
func WithConfirmer(c flow.Confirmer) RunnerOption {
	return func(r *Runner) { r.confirm = c }
}

// WithObserver sets the port that reports each command and dependency step, so a
// host can show progress. It is optional.
func WithObserver(o flow.Observer) RunnerOption {
	return func(r *Runner) { r.observer = o }
}

// WithCredentialSink sets the port for secret steps. Without it, a secret step
// fails closed.
func WithCredentialSink(s flow.CredentialSink) RunnerOption {
	return func(r *Runner) { r.sink = s }
}

// NewRunner returns a Runner. A nil svc disables service registration.
func NewRunner(exec flow.Execer, deps flow.DependencyResolver, svc *service.Store, opts ...RunnerOption) *Runner {
	r := &Runner{exec: exec, deps: deps, svc: svc, clk: clock.System{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run runs the flow with config and then registers the declared service. A failed
// flow registers nothing, so no workload is recorded that was not stood up.
func (r *Runner) Run(ctx context.Context, pb Playbook, config map[string]any) (Result, error) {
	f, err := pb.Spec.DecodeFlow()
	if err != nil {
		return Result{}, err
	}
	interp := flow.New(flow.WithExec(r.exec), flow.WithDependencies(r.deps), flow.WithConfirm(r.confirm),
		flow.WithCredentialSink(r.sink), flow.WithObserver(r.observer))
	out, err := interp.Run(ctx, f, config)
	if err != nil {
		return Result{}, err
	}
	res := Result{Output: out}
	if pb.Spec.Service != nil {
		svc, err := r.register(ctx, pb, out)
		if err != nil {
			return Result{}, fault.Wrap(fault.Terminal, "playbook_register",
				err)
		}
		res.Service = svc
	}
	return res, nil
}

// register records the service from the flow result, an object with name (or
// service), url, externalID (or id) and an optional address object. The name
// defaults to the playbook's.
func (r *Runner) register(ctx context.Context, pb Playbook, out any) (*service.Service, error) {
	if r.svc == nil {
		return nil, nil
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fault.New(fault.Terminal, "playbook_no_result",
			"playbook: declares a service but its flow did not return an object to register")
	}
	name := firstString(m, "name", "service")
	if name == "" {
		name = pb.Name
	}
	spec := service.Spec{
		Provider:     pb.Spec.Service.Provider,
		Target:       pb.Spec.Service.Target,
		ExternalID:   firstString(m, "externalID", "id"),
		URL:          firstString(m, "url"),
		DesiredState: service.StateRunning,
		Address:      stringMap(m["address"]),
	}
	status := service.Status{
		Phase:       "deployed",
		ObservedURL: spec.URL,
		LastDeploy:  r.clk.Now().UTC().Format(time.RFC3339),
	}
	svc, err := r.svc.Put(ctx, name, spec, status)
	if err != nil {
		return nil, err
	}
	return &svc, nil
}

// SandboxExecer runs a playbook's exec steps in a sandbox.
type SandboxExecer struct{ sb sandbox.Sandbox }

// NewSandboxExecer returns a SandboxExecer over sb.
func NewSandboxExecer(sb sandbox.Sandbox) SandboxExecer { return SandboxExecer{sb: sb} }

// Exec returns the exit code and combined output.
func (e SandboxExecer) Exec(ctx context.Context, req flow.ExecRequest) (flow.ExecResult, error) {
	if e.sb == nil {
		return flow.ExecResult{}, fault.New(fault.Terminal, "playbook_no_sandbox", "playbook: no sandbox configured for exec")
	}
	res, err := e.sb.Exec(ctx, sandbox.Command{Line: req.Command})
	if err != nil {
		return flow.ExecResult{}, err
	}
	return flow.ExecResult{ExitCode: res.ExitCode, Output: res.Output}, nil
}

// ManagerResolver resolves a playbook's dependency steps through a dependency
// manager.
type ManagerResolver struct{ mgr *dependency.Manager }

// NewManagerResolver returns a ManagerResolver over mgr.
func NewManagerResolver(mgr *dependency.Manager) ManagerResolver { return ManagerResolver{mgr: mgr} }

// Resolve installs the named dependency if needed and returns its path.
func (m ManagerResolver) Resolve(ctx context.Context, name string) (string, error) {
	if m.mgr == nil {
		return "", fault.New(fault.Terminal, "playbook_no_deps", "playbook: no dependency manager configured")
	}
	got, err := m.mgr.Resolve(ctx, name)
	if err != nil {
		return "", err
	}
	return got.Path, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// stringMap drops non-string values, so the supervisor replays only string
// addressing.
func stringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, val := range m {
		if s, ok := val.(string); ok && s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	_ flow.Execer             = SandboxExecer{}
	_ flow.DependencyResolver = ManagerResolver{}
)
