// Package dispatch is the agent's single execution chokepoint. Every action the
// agent takes - a model call, a tool call, an MCP call, a built-in command -
// flows through Dispatcher.Govern, where governance, tracing, structured logging,
// event emission, and hooks are applied once. Callers bring their own work as a
// closure and inherit all of it; no call site writes its own instrumentation.
//
// The waist is payload-agnostic on purpose. It governs an action's metadata (a
// name, a scope) and what it cost (Metering), never the work's typed input or
// output: a model call keeps its llm.Request/Response, a tool keeps its JSON, and
// dispatch knows neither. That is what lets one bracket govern every kind of
// action without coupling to any of them.
//
// This is the narrow waist the cross-cutting concerns hang off: design it
// early so traceability, governance, and replay are uniform rather than
// retrofitted across scattered call sites.
package dispatch

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Action is the governed unit's identity. It is metadata only: Name resolves the
// admission policy and labels the lifecycle event; Scope attributes the action on
// the spine; Trust is how far the work is trusted, which sets the containment a gate
// requires before it runs. The work's typed input and output stay with the caller, so
// dispatch never depends on a tool's or a model's types.
type Action struct {
	Name  string
	Scope state.Scope
	// Trust classifies the work so a containment gate can refuse it on a host that
	// cannot isolate it. The zero value is sandbox.TrustTrusted: an action carries the
	// agent's own trust unless a call site marks it as model-authored or external.
	Trust sandbox.Trust
	// Goal is the id of the goal this action runs under, recorded on the lifecycle
	// event so a run's per-goal governance posture (which child hit a boundary) is
	// derivable from the stream. Empty when a call site does not run under a goal.
	Goal string
}

// Metering is what a governed unit reports back for accounting and the end event.
// The Dispatcher sees only this, never the unit's typed result.
type Metering struct {
	Tokens int
	Cost   float64
}

// Admitter governs an action before any side effect: capability, budget, and
// approval checks live here. Returning a non-nil error rejects the action; use
// fault classes (e.g. NeedsApproval, BudgetExceeded) so callers can react.
type Admitter interface {
	Admit(ctx context.Context, a Action) error
}

// Event is one record on the action lifecycle, appended to the spine.
type Event struct {
	Type   string // EventStart, EventEnd, or EventRejected
	Action string
	Call   int64 // correlation id pairing one invocation's start with its end or rejection
	Scope  state.Scope
	Trust  string // the work's trust level, recorded so a run's containment posture is auditable
	Goal   string // id of the goal the action ran under, empty when it ran under no goal
	At     int64  // unix nanos, from the dispatcher's clock
	Err    string // fault class on failure; empty on success
}

// Event types.
const (
	EventStart    = "dispatch.start"
	EventEnd      = "dispatch.end"
	EventRejected = "dispatch.rejected"
)

// EventSink appends action-lifecycle events. The event spine implements this
// (internal/spinesink); a DiscardSink or MemorySink is used in tests and headless runs.
type EventSink interface {
	Append(ctx context.Context, e Event) error
}

// Hook observes dispatch without being a call site, so new cross-cutting
// behaviour (adversary review, dry-run, redaction) is registered, not edited
// into every action. Before may reject by returning an error. After runs, in
// reverse order, for each hook whose Before succeeded - including on rejection
// or work failure - so a hook can pair Before/After like acquire/release. After
// receives the Metering the work reported (zero on a rejection).
type Hook interface {
	Before(ctx context.Context, a Action) error
	After(ctx context.Context, a Action, m Metering, err error)
}

// Dispatcher is the chokepoint: a per-run governance context that any number of
// actions Govern through, so model calls, tool calls, and verifications all land
// on one admitter and one event stream. Construct it with New.
type Dispatcher struct {
	admit  Admitter
	events EventSink
	ob     *observe.Observability
	clk    clock.Clock
	hooks  []Hook
	// tokens and actions are the two lifecycle counters, resolved once at construction
	// instead of by string name on every governed call. Govern is the one chokepoint
	// every tool, model, and spawn flows through, so a per-call Meter.Counter lookup is
	// hot; a wired OTel adapter does real work to resolve an instrument by name.
	tokens  observe.Counter
	actions observe.Counter
	// calls assigns each Govern invocation a correlation id, monotonic within this
	// dispatcher, so an action's start and its end or rejection are pairable. It is a
	// counter, not a random id, so a replay reproduces the same ids.
	calls atomic.Int64
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithAdmitter sets the governance gate (default: AllowAll).
func WithAdmitter(a Admitter) Option { return func(d *Dispatcher) { d.admit = a } }

// WithEventSink sets the event spine sink (default: DiscardSink).
func WithEventSink(s EventSink) Option { return func(d *Dispatcher) { d.events = s } }

// WithObservability sets the logger and tracer (default: observe.Default()).
func WithObservability(o *observe.Observability) Option { return func(d *Dispatcher) { d.ob = o } }

// WithClock sets the time source (default: clock.System).
func WithClock(c clock.Clock) Option { return func(d *Dispatcher) { d.clk = c } }

// WithHook appends a lifecycle hook.
func WithHook(h Hook) Option { return func(d *Dispatcher) { d.hooks = append(d.hooks, h) } }

// New builds a Dispatcher, filling in standalone defaults so it is usable with
// zero configuration.
func New(opts ...Option) *Dispatcher {
	d := &Dispatcher{
		admit:  AllowAll{},
		events: DiscardSink{},
		ob:     observe.Default(),
		clk:    clock.System{},
	}
	for _, o := range opts {
		o(d)
	}
	// Resolve the lifecycle counters once, after options so WithObservability's meter
	// is the one used. Govern reuses these instead of a per-call name lookup.
	d.tokens = d.ob.Meter.Counter("dispatch.tokens")
	d.actions = d.ob.Meter.Counter("dispatch.actions")
	return d
}

// Govern runs work through the full waist: Before hooks and admission, then (if
// admitted) a traced, logged, event-bracketed execution, then After hooks unwound
// in reverse for the hooks that entered (see Hook). work is opaque: it performs
// the caller's typed action, captures its result in the caller's own closure, and
// returns only the Metering to charge. The returned error is work's error (or the
// rejection that stopped it from running).
func (d *Dispatcher) Govern(ctx context.Context, a Action, work func(context.Context) (Metering, error)) error {
	ctx, span := d.ob.Tracer.Start(ctx, "dispatch:"+a.Name)
	defer span.End()
	span.SetAttr("action", a.Name)

	// One correlation id for this invocation, so its start and its end or rejection
	// can be paired in the recorded stream even when the same action runs many times.
	call := d.calls.Add(1)

	// Run Before hooks, remembering how many entered so After unwinds exactly
	// that set in reverse, even if a later Before, admission, or the work fails.
	entered := 0
	for _, h := range d.hooks {
		if err := h.Before(ctx, a); err != nil {
			return d.rejected(ctx, a, err, span, entered, call)
		}
		entered++
	}
	if err := d.admit.Admit(ctx, a); err != nil {
		return d.rejected(ctx, a, err, span, entered, call)
	}

	d.emit(ctx, Event{Type: EventStart, Action: a.Name, Call: call, Scope: a.Scope, Trust: a.Trust.String(), Goal: a.Goal, At: d.clk.Now().UnixNano()})
	d.ob.Log.Info(ctx, "dispatch start", observe.String("action", a.Name), observe.String("trust", a.Trust.String()))

	m, err := d.labelled(ctx, a, call, work)

	end := Event{Type: EventEnd, Action: a.Name, Call: call, Scope: a.Scope, Trust: a.Trust.String(), Goal: a.Goal, At: d.clk.Now().UnixNano()}
	outcome := "ok"
	if err != nil {
		class := fault.Classify(err)
		end.Err = string(class)
		outcome = "error"
		span.RecordError(err)
		d.ob.Log.Warn(ctx, "dispatch failed",
			observe.String("action", a.Name), observe.String("class", string(class)))
	} else {
		span.SetAttr("tokens", m.Tokens)
		d.tokens.Add(ctx, int64(m.Tokens), observe.String("action", a.Name))
	}
	d.actions.Add(ctx, 1, observe.String("action", a.Name), observe.String("outcome", outcome))
	d.emit(ctx, end)
	d.unwind(ctx, a, m, err, entered)
	return err
}

// labelled runs work under pprof labels naming the action, so every CPU sample
// taken inside it and every goroutine it starts is attributable to this
// invocation rather than to an anonymous stack. The waist is the only place that
// knows what an action is, which is why the labels are applied here and nowhere
// else.
//
// The guard is not an optimization detail: pprof.Do allocates a label map per
// call, and the waist runs on every tool call, so an unprofiled process must take
// the direct path. dispatch/alloc_test.go holds that ceiling.
func (d *Dispatcher) labelled(ctx context.Context, a Action, call int64, work func(context.Context) (Metering, error)) (Metering, error) {
	if !diag.Profiling() {
		return work(ctx)
	}
	var (
		m   Metering
		err error
	)
	diag.Labeled(ctx, func(ctx context.Context) { m, err = work(ctx) }, actionLabels(a, call)...)
	return m, err
}

// actionLabels is the label set a governed action carries into a profile. Goal is
// omitted when empty rather than emitted as an empty string, so a profile grouped
// by goal does not grow a bucket of everything that ran under none. There is no
// run id yet: the waist has no ambient run context to read one from, and inventing
// a second carrier for it here would be the wrong place to put it.
func actionLabels(a Action, call int64) []string {
	kv := []string{
		"action", a.Name,
		"trust", a.Trust.String(),
		"call", strconv.FormatInt(call, 10),
	}
	if a.Goal != "" {
		kv = append(kv, "goal", a.Goal)
	}
	return kv
}

// rejected records a pre-execution rejection (a Before hook or admission) and
// unwinds the hooks that already entered. The work never ran, so the metering is
// zero.
func (d *Dispatcher) rejected(ctx context.Context, a Action, err error, span observe.Span, entered int, call int64) error {
	class := fault.Classify(err)
	span.RecordError(err)
	d.emit(ctx, Event{Type: EventRejected, Action: a.Name, Call: call, Scope: a.Scope, Trust: a.Trust.String(), Goal: a.Goal, At: d.clk.Now().UnixNano(), Err: string(class)})
	d.ob.Log.Warn(ctx, "dispatch rejected",
		observe.String("action", a.Name), observe.String("class", string(class)))
	d.actions.Add(ctx, 1, observe.String("action", a.Name), observe.String("outcome", "rejected"))
	d.unwind(ctx, a, Metering{}, err, entered)
	return err
}

// unwind runs After for the first n hooks (those whose Before succeeded), in
// reverse order, so Before/After pair like a defer stack.
func (d *Dispatcher) unwind(ctx context.Context, a Action, m Metering, err error, n int) {
	for i := n - 1; i >= 0; i-- {
		d.hooks[i].After(ctx, a, m, err)
	}
}

// emit appends an event, logging (but not failing the dispatch on) a sink error.
func (d *Dispatcher) emit(ctx context.Context, e Event) {
	if err := d.events.Append(ctx, e); err != nil {
		d.ob.Log.Warn(ctx, "event sink append failed",
			observe.String("event", e.Type), observe.String("action", e.Action))
	}
}

// AllowAll is an Admitter that admits every action; the standalone default
// until the governor is wired in.
type AllowAll struct{}

// Admit implements Admitter.
func (AllowAll) Admit(context.Context, Action) error { return nil }

// DiscardSink is an EventSink that drops events; the standalone default.
type DiscardSink struct{}

// Append implements EventSink.
func (DiscardSink) Append(context.Context, Event) error { return nil }

// Compile-time checks for the default implementations.
var (
	_ Admitter  = AllowAll{}
	_ EventSink = DiscardSink{}
)
