package flow

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// HTTPRequest is one outbound request the interpreter asks the host to perform. It
// is a value type, free of net/http, so the interpreter stays transport-agnostic
// and fully testable: the host adapts its governed transport to this port.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Query   map[string]string
	Body    []byte
}

// HTTPResponse is the result of a request, decoded for use by later steps. Body is
// the parsed JSON value when the payload is JSON, otherwise the raw text; Raw is the
// unparsed bytes, against which the payload cap is checked.
type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    any
	Raw     []byte
}

// HTTPDoer performs the http steps of a flow. The host's implementation is where
// egress confinement, credential injection, rate limiting, and retries live, so the
// interpreter never reaches the network itself.
type HTTPDoer interface {
	Do(ctx context.Context, req HTTPRequest) (HTTPResponse, error)
}

// ToolCaller performs the call steps of a flow: it invokes another tool or
// extension by name. The host gates the call at the dispatch boundary, so a flow
// cannot call anything the run is not authorised for.
type ToolCaller interface {
	Call(ctx context.Context, tool string, input json.RawMessage) (any, error)
}

// ExecRequest is one command an exec step asks the host to run. It is a value type,
// free of os/exec, so the interpreter never spawns a process itself: the host runs the
// command through the sandbox, where confinement, egress policy, and timeouts live.
type ExecRequest struct {
	Command string
}

// ExecResult is the outcome of a command: its exit code and combined output.
type ExecResult struct {
	ExitCode int
	Output   string
}

// Execer performs the exec steps of a flow by running a command through the sandbox.
// Without it, an exec step fails closed, so a flow can never reach a process except
// through a host that confines it.
type Execer interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
}

// DependencyResolver performs the dependency steps of a flow: it ensures an external
// program is present (provisioning a pinned build when missing) and returns the path to
// run it. The host wires the dependency manager, so a flow declares what it needs
// without knowing how it is obtained.
type DependencyResolver interface {
	Resolve(ctx context.Context, name string) (path string, err error)
}

// Confirmer performs the confirm steps of a flow: it shows the user a message and waits
// for them to approve before the flow continues. The host wires how that is asked (an
// interactive terminal prompt for a person, a fail-closed instruction for a
// non-interactive run). Returning a non-nil error stops the flow, so a declined or
// unanswerable confirmation aborts rather than proceeding without consent.
type Confirmer interface {
	Confirm(ctx context.Context, message string) error
}

// Limits bound a flow's resource use so a runtime-authored flow cannot wedge or
// amplify. A zero field takes the interpreter's default; DefaultLimits documents
// those. They are enforced as terminal faults: exceeding one stops the flow, it is
// never retried into the same wall.
type Limits struct {
	// MaxSteps caps total step executions across the whole run (loop bodies included).
	MaxSteps int `json:"maxSteps,omitempty"`
	// MaxLoopIterations caps total loop iterations across the whole run.
	MaxLoopIterations int `json:"maxLoopIterations,omitempty"`
	// TimeoutMillis caps wall-clock duration, measured against the injected clock.
	TimeoutMillis int `json:"timeoutMillis,omitempty"`
	// MaxPayloadBytes caps a single http response body; a larger response fails closed.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`
}

// DefaultLimits is the conservative envelope applied when a flow or the interpreter
// sets no override. The numbers are deliberately modest: a flow that needs more is
// declaring an unusual shape and should say so explicitly.
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

// Interpreter runs flows. It holds the injected effect ports and the default limits;
// it is safe for concurrent use because Run keeps all mutable run state local.
type Interpreter struct {
	http     HTTPDoer
	tools    ToolCaller
	exec     Execer
	deps     DependencyResolver
	confirm  Confirmer
	observer Observer
	clk      clock.Clock
	limits   Limits
}

// Option configures an Interpreter.
type Option func(*Interpreter)

// WithHTTP sets the port that performs http steps. Without it, an http step fails
// closed.
func WithHTTP(d HTTPDoer) Option { return func(i *Interpreter) { i.http = d } }

// WithTools sets the port that performs call steps. Without it, a call step fails
// closed.
func WithTools(c ToolCaller) Option { return func(i *Interpreter) { i.tools = c } }

// WithExec sets the port that runs exec steps through the sandbox. Without it, an exec
// step fails closed.
func WithExec(e Execer) Option { return func(i *Interpreter) { i.exec = e } }

// WithDependencies sets the port that resolves dependency steps. Without it, a
// dependency step fails closed.
func WithDependencies(d DependencyResolver) Option { return func(i *Interpreter) { i.deps = d } }

// WithConfirm sets the port that asks the user to approve confirm steps. Without it, a
// confirm step fails closed.
func WithConfirm(c Confirmer) Option { return func(i *Interpreter) { i.confirm = c } }

// WithObserver sets the port that watches each observable step as it runs, so a host can
// show progress while a flow executes. Without it, a flow still runs; nothing is reported.
func WithObserver(o Observer) Option { return func(i *Interpreter) { i.observer = o } }

// WithClock sets the time source the timeout cap measures against (default
// clock.System). Tests pass a Manual clock for determinism.
func WithClock(c clock.Clock) Option { return func(i *Interpreter) { i.clk = c } }

// WithLimits overrides the default resource caps.
func WithLimits(l Limits) Option { return func(i *Interpreter) { i.limits = l } }

// New builds an interpreter. With no ports, it can still run pure flows (transform,
// condition, loop, return); http and call steps require their ports.
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

// Run executes a flow against the given configuration and returns its result. The
// result is whatever a return step yields; a flow that falls off the end with no
// return yields nil. config is exposed to expressions as "config"; step outputs
// accumulate under "steps".
func (in *Interpreter) Run(ctx context.Context, f Flow, config map[string]any) (any, error) {
	if err := f.Validate(); err != nil {
		return nil, err
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
	if _, err := r.execSteps(ctx, f.Steps, root); err != nil {
		return nil, err
	}
	return r.result, nil
}

// run is the mutable state of one Run: budgets consumed, the accumulating step
// outputs, and the return result. It never escapes Run, so an Interpreter is
// reusable and concurrency-safe.
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

// observe reports a step event to the interpreter's observer when one is configured, so
// progress reporting is a no-op cost when nothing is watching.
func (r *run) observe(ev StepEvent) {
	if r.in.observer != nil {
		r.in.observer.Step(ev)
	}
}

// execSteps runs a sequence until it ends or a return fires. It returns whether a
// return propagated, so an enclosing loop or condition can stop early too.
func (r *run) execSteps(ctx context.Context, steps []Step, s *scope) (bool, error) {
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

// checkBudget enforces the step count alongside the time and cancellation checks
// before a step runs, so a runaway flow stops promptly rather than after the fact.
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

// checkDeadline enforces the time cap and context cancellation without charging a
// step. A loop calls it every iteration so an empty- or collect-only body (which
// runs no inner steps) still observes the deadline and a cancelled context, and
// cancellation is classified as Cancelled so a caller can tell it apart from a
// genuine terminal failure.
func (r *run) checkDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Cancelled, "flow_cancelled", err)
	}
	if r.in.clk.Now().After(r.deadline) {
		return fault.New(fault.Terminal, "flow_timeout", "flow: exceeded time cap")
	}
	return nil
}

func (r *run) execStep(ctx context.Context, st Step, s *scope) error {
	var (
		out any
		err error
	)
	switch st.Op {
	case OpHTTP:
		out, err = r.execHTTP(ctx, st.HTTP, s)
	case OpTransform:
		out, err = r.execTransform(st.Transform, s)
	case OpCondition:
		err = r.execCondition(ctx, st.Condition, s)
	case OpLoop:
		out, err = r.execLoop(ctx, st.Loop, s)
	case OpCall:
		out, err = r.execCall(ctx, st.Call, s)
	case OpReturn:
		return r.execReturn(st.Return, s)
	case OpAssert:
		return r.execAssert(st.Assert, s)
	case OpExec:
		out, err = r.execExec(ctx, st.Exec, s)
	case OpDependency:
		out, err = r.execDependency(ctx, st.Dependency, s)
	case OpConfirm:
		return r.execConfirm(ctx, st.Confirm, s)
	default:
		return fault.New(fault.Terminal, "flow_unknown_op", "flow: unknown op "+string(st.Op))
	}
	if err != nil {
		return err
	}
	// Record a value-producing step's output so later steps can reference it. Condition
	// and assert produce no value (they branch or verify), so they record nothing.
	if st.ID != "" && st.Op != OpCondition {
		r.stepsOut[st.ID] = out
	}
	return nil
}

func (r *run) execReturn(a *ReturnAction, s *scope) error {
	v, err := renderBody(a.Value, s)
	if err != nil {
		return err
	}
	r.result = v
	r.returned = true
	return nil
}

// renderBody deep-templates a JSON body value against the scope. An empty body
// yields nil.
func renderBody(raw json.RawMessage, s *scope) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_bad_body", err)
	}
	out, err := deepTemplate(v, s)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_template", err)
	}
	return out, nil
}
