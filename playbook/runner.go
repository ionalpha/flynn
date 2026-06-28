package playbook

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dependency"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/flow"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/service"
)

// Result is the outcome of a playbook run: the flow's return value, and the supervised
// Service the run registered (nil when the playbook declares none or the result carried no
// workload to track).
type Result struct {
	Output  any
	Service *service.Service
}

// Runner executes a playbook's flow with the effect ports wired and, on success, registers
// the supervised Service the playbook declares. It holds no playbook knowledge: it runs
// whatever flow the spec carries, and the only things it can do are the ones its ports
// allow (run a command in the sandbox, resolve a dependency, write a service record).
type Runner struct {
	exec flow.Execer
	deps flow.DependencyResolver
	svc  *service.Store
	clk  clock.Timing
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithClock sets the time source used to stamp a registered service (default clock.System).
func WithClock(c clock.Timing) RunnerOption {
	return func(r *Runner) {
		if c != nil {
			r.clk = c
		}
	}
}

// NewRunner builds a playbook runner. exec runs the flow's exec steps (through the
// sandbox); deps resolves its dependency steps (through the dependency manager); svc is the
// service store a successful run registers its workload in (nil disables registration).
func NewRunner(exec flow.Execer, deps flow.DependencyResolver, svc *service.Store, opts ...RunnerOption) *Runner {
	r := &Runner{exec: exec, deps: deps, svc: svc, clk: clock.System{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run executes the playbook's flow with config exposed to it, then registers the declared
// supervised Service from the flow's result. A flow failure is returned as-is; the service
// is registered only after the flow succeeds, so a failed run never records a workload it
// did not finish standing up.
func (r *Runner) Run(ctx context.Context, pb Playbook, config map[string]any) (Result, error) {
	f, err := pb.Spec.DecodeFlow()
	if err != nil {
		return Result{}, err
	}
	interp := flow.New(flow.WithExec(r.exec), flow.WithDependencies(r.deps))
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

// register reads the live workload fields from the flow result and records a supervised
// Service, so what the playbook stood up is held in its desired state afterward. The flow
// result is expected to be an object carrying at least a name and a url; addressing the
// supervisor replays is taken verbatim from an "address" object when present.
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

// SandboxExecer adapts a sandbox to the flow's Execer port, so a playbook's exec steps run
// confined in the sandbox rather than spawning a process directly.
type SandboxExecer struct{ sb sandbox.Sandbox }

// NewSandboxExecer wraps a sandbox as a flow command runner.
func NewSandboxExecer(sb sandbox.Sandbox) SandboxExecer { return SandboxExecer{sb: sb} }

// Exec runs the command through the sandbox and returns its exit code and combined output.
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

// ManagerResolver adapts the dependency manager to the flow's DependencyResolver port, so a
// playbook's dependency steps ensure a program is present and yield the path to run it.
type ManagerResolver struct{ mgr *dependency.Manager }

// NewManagerResolver wraps a dependency manager as a flow dependency resolver.
func NewManagerResolver(mgr *dependency.Manager) ManagerResolver { return ManagerResolver{mgr: mgr} }

// Resolve satisfies the named dependency and returns the path to run it.
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

// firstString returns the first key in m whose value is a non-empty string.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// stringMap reads a flow result's "address" object into a string map, dropping any
// non-string value, so the supervisor only ever replays string addressing.
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

// guards: the adapters satisfy the flow ports.
var (
	_ flow.Execer             = SandboxExecer{}
	_ flow.DependencyResolver = ManagerResolver{}
)
