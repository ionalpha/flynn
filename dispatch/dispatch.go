// Package dispatch is the agent's single execution chokepoint. Every action the
// agent takes — a tool call, an MCP call, a built-in command — flows through
// Dispatcher.Dispatch, where governance, tracing, structured logging, event
// emission, and hooks are applied once. Tool authors implement a Handler behind
// a port and inherit all of it; no call site writes its own instrumentation.
//
// This is the substrate waist the cross-cutting concerns hang off: design it
// early so traceability, governance, and replay are uniform rather than
// retrofitted across scattered call sites.
package dispatch

import (
	"context"
	"log/slog"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/obs"
	"github.com/ionalpha/flynn/state"
)

// Action is one unit of work the agent asks to perform. It is data: a name that
// resolves to a Handler, typed input, and the scope it runs in.
type Action struct {
	Name  string
	Input map[string]any
	Scope state.Scope
}

// Result is the outcome of a successfully executed Action.
type Result struct {
	Output map[string]any
	Tokens int
	Cost   float64
}

// Handler executes a resolved Action behind a port (provider, browser, channel,
// MCP server, …). It must not assume any cross-cutting concern is its job; the
// Dispatcher owns those.
type Handler interface {
	Handle(ctx context.Context, a Action) (Result, error)
}

// HandlerFunc adapts an ordinary function to Handler.
type HandlerFunc func(ctx context.Context, a Action) (Result, error)

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, a Action) (Result, error) { return f(ctx, a) }

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
	Scope  state.Scope
	At     int64  // unix nanos, from the dispatcher's clock
	Err    string // fault class on failure; empty on success
}

// Event types.
const (
	EventStart    = "dispatch.start"
	EventEnd      = "dispatch.end"
	EventRejected = "dispatch.rejected"
)

// EventSink appends action-lifecycle events. The structural event spine
// implements this; until it lands, a DiscardSink or MemorySink is used.
type EventSink interface {
	Append(ctx context.Context, e Event) error
}

// Hook observes dispatch without being a call site, so new cross-cutting
// behaviour (adversary review, dry-run, redaction) is registered, not edited
// into every action. Before may reject by returning an error. After runs, in
// reverse order, for each hook whose Before succeeded — including on rejection
// or handler failure — so a hook can pair Before/After like acquire/release.
type Hook interface {
	Before(ctx context.Context, a Action) error
	After(ctx context.Context, a Action, r Result, err error)
}

// Dispatcher is the chokepoint. Construct it with New.
type Dispatcher struct {
	handler Handler
	admit   Admitter
	events  EventSink
	ob      *obs.Observability
	clk     clock.Clock
	hooks   []Hook
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithAdmitter sets the governance gate (default: AllowAll).
func WithAdmitter(a Admitter) Option { return func(d *Dispatcher) { d.admit = a } }

// WithEventSink sets the event spine sink (default: DiscardSink).
func WithEventSink(s EventSink) Option { return func(d *Dispatcher) { d.events = s } }

// WithObservability sets the logger and tracer (default: obs.Default()).
func WithObservability(o *obs.Observability) Option { return func(d *Dispatcher) { d.ob = o } }

// WithClock sets the time source (default: clock.System).
func WithClock(c clock.Clock) Option { return func(d *Dispatcher) { d.clk = c } }

// WithHook appends a lifecycle hook.
func WithHook(h Hook) Option { return func(d *Dispatcher) { d.hooks = append(d.hooks, h) } }

// New builds a Dispatcher around handler, filling in standalone defaults so it
// is usable with zero configuration.
func New(handler Handler, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		handler: handler,
		admit:   AllowAll{},
		events:  DiscardSink{},
		ob:      obs.Default(),
		clk:     clock.System{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Dispatch runs an action through the full waist: Before hooks and admission,
// then (if admitted) a traced, logged, event-bracketed execution, then After
// hooks unwound in reverse for the hooks that entered (see Hook).
func (d *Dispatcher) Dispatch(ctx context.Context, a Action) (Result, error) {
	ctx, span := d.ob.Tracer.Start(ctx, "dispatch:"+a.Name)
	defer span.End()
	span.SetAttr("action", a.Name)

	// Run Before hooks, remembering how many entered so After unwinds exactly
	// that set in reverse, even if a later Before, admission, or the handler fails.
	entered := 0
	for _, h := range d.hooks {
		if err := h.Before(ctx, a); err != nil {
			return d.rejected(ctx, a, err, span, entered)
		}
		entered++
	}
	if err := d.admit.Admit(ctx, a); err != nil {
		return d.rejected(ctx, a, err, span, entered)
	}

	d.emit(ctx, Event{Type: EventStart, Action: a.Name, Scope: a.Scope, At: d.clk.Now().UnixNano()})
	d.ob.Log.InfoContext(ctx, "dispatch start", slog.String("action", a.Name))

	r, err := d.handler.Handle(ctx, a)

	end := Event{Type: EventEnd, Action: a.Name, Scope: a.Scope, At: d.clk.Now().UnixNano()}
	if err != nil {
		class := fault.Classify(err)
		end.Err = string(class)
		span.RecordError(err)
		d.ob.Log.WarnContext(ctx, "dispatch failed",
			slog.String("action", a.Name), slog.String("class", string(class)))
	} else {
		span.SetAttr("tokens", r.Tokens)
	}
	d.emit(ctx, end)
	d.unwind(ctx, a, r, err, entered)
	return r, err
}

// rejected records a pre-execution rejection (a Before hook or admission) and
// unwinds the hooks that already entered. The handler never ran, so the result
// is zero.
func (d *Dispatcher) rejected(ctx context.Context, a Action, err error, span obs.Span, entered int) (Result, error) {
	class := fault.Classify(err)
	span.RecordError(err)
	d.emit(ctx, Event{Type: EventRejected, Action: a.Name, Scope: a.Scope, At: d.clk.Now().UnixNano(), Err: string(class)})
	d.ob.Log.WarnContext(ctx, "dispatch rejected",
		slog.String("action", a.Name), slog.String("class", string(class)))
	d.unwind(ctx, a, Result{}, err, entered)
	return Result{}, err
}

// unwind runs After for the first n hooks (those whose Before succeeded), in
// reverse order, so Before/After pair like a defer stack.
func (d *Dispatcher) unwind(ctx context.Context, a Action, r Result, err error, n int) {
	for i := n - 1; i >= 0; i-- {
		d.hooks[i].After(ctx, a, r, err)
	}
}

// emit appends an event, logging (but not failing the dispatch on) a sink error.
func (d *Dispatcher) emit(ctx context.Context, e Event) {
	if err := d.events.Append(ctx, e); err != nil {
		d.ob.Log.WarnContext(ctx, "event sink append failed",
			slog.String("event", e.Type), slog.String("action", e.Action))
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
	_ Handler   = HandlerFunc(nil)
)
