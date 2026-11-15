package flow

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// HTTPRequest is one request the interpreter asks the host to perform. It is free of
// net/http, so the host adapts its governed transport and tests need no network.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Query   map[string]string
	Body    []byte
}

// HTTPResponse is a request's result. Body is the parsed JSON when the payload is
// JSON, else the text; Raw is the bytes the payload cap is checked against.
type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    any
	Raw     []byte
}

// HTTPDoer performs http steps. Egress confinement, credential injection, rate
// limits and retries are in the host's implementation, so the interpreter never
// reaches the network.
type HTTPDoer interface {
	Do(ctx context.Context, req HTTPRequest) (HTTPResponse, error)
}

// ToolCaller performs call steps. The host gates each call at dispatch, so a flow
// cannot call anything the run is not authorized for.
type ToolCaller interface {
	Call(ctx context.Context, tool string, input json.RawMessage) (any, error)
}

// ExecRequest is one command for the host to run. It is free of os/exec, so the
// interpreter never spawns a process; the host runs it in the sandbox.
type ExecRequest struct {
	Command string
}

// ExecResult is a command's exit code and combined output.
type ExecResult struct {
	ExitCode int
	Output   string
}

// Execer runs exec steps through the sandbox. Without one an exec step fails
// closed, so a flow reaches a process only through a confining host.
type Execer interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
}

// DependencyResolver ensures an external program is present and returns its path.
// The host wires the dependency manager, so a flow declares what it needs without
// knowing how it is obtained.
type DependencyResolver interface {
	Resolve(ctx context.Context, name string) (path string, err error)
}

// CredentialSink materializes a referenced secret into a target's secret store
// (provider secrets, a workload's environment). It resolves the value through the
// host's secret source, so the value never enters the flow. An error fails the
// step; without a sink a secret step fails closed.
type CredentialSink interface {
	Put(ctx context.Context, sink, ref string, target map[string]string) error
}

// Confirmer shows the user a message and waits for approval: a terminal prompt for
// a person, a fail-closed instruction for a non-interactive run. An error stops the
// flow, so a declined or unanswerable confirmation never proceeds.
type Confirmer interface {
	Confirm(ctx context.Context, message string) error
}

// Limits bound a flow's resource use so a runtime-authored flow cannot wedge or
// amplify. A zero field takes the default (DefaultLimits). Exceeding one is a
// terminal fault, never retried.
type Limits struct {
	// MaxSteps caps step executions across the run, loop bodies included.
	MaxSteps int `json:"maxSteps,omitempty"`
	// MaxLoopIterations caps loop iterations across the run.
	MaxLoopIterations int `json:"maxLoopIterations,omitempty"`
	// TimeoutMillis caps wall-clock time in milliseconds on the injected clock.
	TimeoutMillis int `json:"timeoutMillis,omitempty"`
	// MaxPayloadBytes caps one http response body; a larger one fails closed.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`
}

// DefaultLimits apply when no override is set. They are modest, so a flow that
// needs more says so explicitly.
var DefaultLimits = Limits{
	MaxSteps:          256,
	MaxLoopIterations: 1000,
	TimeoutMillis:     30_000,
	MaxPayloadBytes:   5 << 20, // 5 MiB
}

// merge returns l with any zero field filled from base.
func (l Limits) merge(base Limits) Limits {
	if l.MaxSteps == 0 {
		l.MaxSteps = base.MaxSteps
	}
	if l.MaxLoopIterations == 0 {
		l.MaxLoopIterations = base.MaxLoopIterations
	}
	if l.TimeoutMillis == 0 {
		l.TimeoutMillis = base.TimeoutMillis
	}
	if l.MaxPayloadBytes == 0 {
		l.MaxPayloadBytes = base.MaxPayloadBytes
	}
	return l
}

// Interpreter runs flows with injected effect ports and default limits. It is safe
// for concurrent use because Run keeps run state local.
type Interpreter struct {
	http     HTTPDoer
	tools    ToolCaller
	exec     Execer
	deps     DependencyResolver
	confirm  Confirmer
	sink     CredentialSink
	observer Observer
	clk      clock.Clock
	limits   Limits
}

// Option configures an Interpreter.
type Option func(*Interpreter)

// WithHTTP sets the http step port. Without it an http step fails closed.
func WithHTTP(d HTTPDoer) Option { return func(i *Interpreter) { i.http = d } }

// WithTools sets the call step port. Without it a call step fails closed.
func WithTools(c ToolCaller) Option { return func(i *Interpreter) { i.tools = c } }

// WithExec sets the sandboxed exec port. Without it an exec step fails closed.
func WithExec(e Execer) Option { return func(i *Interpreter) { i.exec = e } }

// WithDependencies sets the dependency port. Without it a dependency step fails
// closed.
func WithDependencies(d DependencyResolver) Option { return func(i *Interpreter) { i.deps = d } }

// WithConfirm sets the confirm port. Without it a confirm step fails closed.
func WithConfirm(c Confirmer) Option { return func(i *Interpreter) { i.confirm = c } }

// WithCredentialSink sets the secret step port. Without it a secret step fails
// closed.
func WithCredentialSink(s CredentialSink) Option { return func(i *Interpreter) { i.sink = s } }

// WithObserver sets the port that watches observable steps, so a host can show
// progress. Without it nothing is reported.
func WithObserver(o Observer) Option { return func(i *Interpreter) { i.observer = o } }

// WithClock sets the time source for the timeout cap (default clock.System).
func WithClock(c clock.Clock) Option { return func(i *Interpreter) { i.clk = c } }

// WithLimits overrides the default resource caps.
func WithLimits(l Limits) Option { return func(i *Interpreter) { i.limits = l } }

// New builds an interpreter. Without ports it runs pure flows (transform, condition,
// loop, return).
func New(opts ...Option) *Interpreter {
	in := &Interpreter{limits: DefaultLimits}
	for _, o := range opts {
		o(in)
	}
	if in.clk == nil {
		in.clk = clock.System{}
	}
	in.limits = in.limits.merge(DefaultLimits)
	return in
}

// Run executes f with config, exposed as "config", and returns what a return step
// yields, or nil if none fires. Step outputs accumulate under "steps".
func (in *Interpreter) Run(ctx context.Context, f Flow, config map[string]any) (any, error) {
	// A Flow built without Decode compiles here, once per Run. The tree stays local
	// to the call, so such a Flow is still safe to run concurrently.
	cf := f.compiled
	if cf == nil {
		var err error
		if cf, err = compileFlow(f); err != nil {
			return nil, err
		}
	}
	limits := in.limits
	if f.Limits != nil {
		limits = f.Limits.merge(in.limits)
	}
	if config == nil {
		config = map[string]any{}
	}
	stepsOut := map[string]any{}
	root := newScope(map[string]any{
		"config": config,
		"steps":  stepsOut,
	})
	r := &run{
		in:       in,
		limits:   limits,
		stepsOut: stepsOut,
		deadline: in.clk.Now().Add(time.Duration(limits.TimeoutMillis) * time.Millisecond),
	}
	if _, err := r.execSteps(ctx, cf.steps, root); err != nil {
		return nil, err
	}
	return r.result, nil
}

// run is one Run's mutable state: budgets, step outputs and the result. It never
// escapes Run, so an Interpreter is reusable and concurrency-safe.
type run struct {
	in       *Interpreter
	limits   Limits
	steps    int
	loops    int
	deadline time.Time
	stepsOut map[string]any
	returned bool
	result   any
}

// observe reports ev to the observer when one is set.
func (r *run) observe(ev StepEvent) {
	if r.in.observer != nil {
		r.in.observer.Step(ev)
	}
}

// execSteps runs steps until they end or a return fires, reporting the return so an
// enclosing loop or condition stops too.
func (r *run) execSteps(ctx context.Context, steps []compiledStep, s *scope) (bool, error) {
	for i := range steps {
		if r.returned {
			return true, nil
		}
		if err := r.checkBudget(ctx); err != nil {
			return false, err
		}
		if err := r.execStep(ctx, steps[i], s); err != nil {
			return false, err
		}
		if r.returned {
			return true, nil
		}
	}
	return false, nil
}

// checkBudget enforces the step count with the time and cancellation checks before
// a step runs, so a runaway flow stops promptly.
func (r *run) checkBudget(ctx context.Context) error {
	if err := r.checkDeadline(ctx); err != nil {
		return err
	}
	r.steps++
	if r.steps > r.limits.MaxSteps {
		return fault.New(fault.Terminal, "flow_max_steps", "flow: exceeded max steps cap")
	}
	return nil
}

// checkDeadline enforces the time cap and cancellation without charging a step. A
// loop calls it each iteration, so a body running no inner steps still sees the
// deadline. Cancellation is classified Cancelled, apart from a terminal failure.
func (r *run) checkDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Cancelled, "flow_cancelled", err)
	}
	if r.in.clk.Now().After(r.deadline) {
		return fault.New(fault.Terminal, "flow_timeout", "flow: exceeded time cap")
	}
	return nil
}

func (r *run) execStep(ctx context.Context, st compiledStep, s *scope) error {
	var (
		out any
		err error
	)
	switch st.op {
	case OpHTTP:
		out, err = r.execHTTP(ctx, st.http, s)
	case OpTransform:
		out, err = r.execTransform(st.transform, s)
	case OpCondition:
		err = r.execCondition(ctx, st.condition, s)
	case OpLoop:
		out, err = r.execLoop(ctx, st.loop, s)
	case OpCall:
		out, err = r.execCall(ctx, st.call, s)
	case OpReturn:
		return r.execReturn(st.ret, s)
	case OpAssert:
		return r.execAssert(st.assert, s)
	case OpExec:
		out, err = r.execExec(ctx, st.exec, s)
	case OpDependency:
		out, err = r.execDependency(ctx, st.dependency, s)
	case OpConfirm:
		return r.execConfirm(ctx, st.confirm, s)
	case OpSecret:
		return r.execSecret(ctx, st.secret, s)
	default:
		return fault.New(fault.Terminal, "flow_unknown_op", "flow: unknown op "+string(st.op))
	}
	if err != nil {
		return err
	}
	// Record a value step's output for later steps. Condition and assert produce no
	// value.
	if st.id != "" && st.op != OpCondition {
		r.stepsOut[st.id] = out
	}
	return nil
}

func (r *run) execReturn(a *compiledReturn, s *scope) error {
	if a.value == nil {
		r.result = nil
		r.returned = true
		return nil
	}
	v, err := a.value.render(s)
	if err != nil {
		return templErr(err)
	}
	r.result = v
	r.returned = true
	return nil
}
