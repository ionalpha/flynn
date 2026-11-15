// Package dispatch is the agent's single execution chokepoint. Every model call, tool
// call, MCP call and built-in command runs through [Dispatcher.Govern], which applies
// admission, hooks, tracing, logging and lifecycle events once, so no call site writes
// its own instrumentation.
//
// The waist sees an action's name, scope and cost ([Metering]) and never its typed input
// or output, so one bracket governs every kind of action without importing their types.
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

// Action is the governed unit's identity. Name selects the admission policy and labels
// the event; Scope attributes the action on the spine.
type Action struct {
	Name  string
	Scope state.Scope
	// Trust lets a containment gate refuse work the host cannot isolate. The zero value
	// is sandbox.TrustTrusted, the agent's own trust.
	Trust sandbox.Trust
	// Goal is the id of the goal the action runs under, or empty. It is recorded so the
	// stream shows which child of a fan-out hit a gate.
	Goal string
}

// Metering is what a governed unit reports for accounting and the end event.
type Metering struct {
	Tokens int
	Cost   float64
}

// Admitter checks an action before any side effect. An error rejects it; return a fault
// class (NeedsApproval, BudgetExceeded) so callers can react.
type Admitter interface {
	Admit(ctx context.Context, a Action) error
}

// Event is one record on the action lifecycle, appended to the spine.
type Event struct {
	Type   string // EventStart, EventEnd, or EventRejected
	Action string
	Call   int64 // correlation id pairing one invocation's start with its end or rejection
	Scope  state.Scope
	Trust  string // the work's trust level
	Goal   string // id of the goal the action ran under, or empty
	At     int64  // unix nanos, from the dispatcher's clock
	Err    string // fault class on failure; empty on success
	// Code is the stable identifier of the rule that refused or the failure. Every gate
	// refuses with the same few classes, so only Code says which gate it was.
	Code string
}

// Event types.
const (
	EventStart    = "dispatch.start"
	EventEnd      = "dispatch.end"
	EventRejected = "dispatch.rejected"
)

// EventSink appends lifecycle events. internal/spinesink writes them to the spine.
type EventSink interface {
	Append(ctx context.Context, e Event) error
}

// Hook adds behaviour to every action (review, dry-run, redaction) without editing call
// sites. Before may reject by returning an error. After runs in reverse order for each
// hook whose Before succeeded, also on rejection or failure, so the two pair like acquire
// and release. After gets zero Metering on a rejection.
type Hook interface {
	Before(ctx context.Context, a Action) error
	After(ctx context.Context, a Action, m Metering, err error)
}

// Dispatcher is one run's governance context: every action it governs shares one
// admitter and one event stream. Construct it with [New].
type Dispatcher struct {
	admit  Admitter
	events EventSink
	ob     *observe.Observability
	clk    clock.Clock
	hooks  []Hook
	// tokens and actions are resolved once, because an OTel adapter does work to look up
	// an instrument by name and Govern runs on every action.
	tokens  observe.Counter
	actions observe.Counter
	// calls numbers each Govern call so its start pairs with its end or rejection. A
	// counter, so a replay reproduces the ids.
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

// New builds a Dispatcher with standalone defaults for anything not configured.
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
	// After options, so WithObservability's meter is the one used.
	d.tokens = d.ob.Meter.Counter("dispatch.tokens")
	d.actions = d.ob.Meter.Counter("dispatch.actions")
	return d
}

// Govern runs Before hooks and admission, then work under a span and start/end events,
// then After hooks in reverse. work keeps its typed result in the caller's closure and
// returns only the Metering to charge. The error is work's, or the rejection.
func (d *Dispatcher) Govern(ctx context.Context, a Action, work func(context.Context) (Metering, error)) error {
	ctx, span := d.ob.Tracer.Start(ctx, "dispatch:"+a.Name)
	defer span.End()
	span.SetAttr("action", a.Name)

	call := d.calls.Add(1)

	// Count entries so After unwinds exactly the hooks that entered.
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
		end.Code = fault.CodeOf(err)
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

// labelled runs work under pprof labels naming the action, so CPU samples and
// goroutines inside it are attributable to this call. pprof.Do allocates a label map per
// call, so an unprofiled process takes the direct path; dispatch/alloc_test.go holds
// that.
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

// actionLabels omits an empty goal so a profile grouped by goal has no bucket for
// actions that ran under none. There is no run id because the waist has no run context
// to read one from.
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

// rejected records a rejection by a Before hook or admission and unwinds the hooks that
// entered, with zero metering.
func (d *Dispatcher) rejected(ctx context.Context, a Action, err error, span observe.Span, entered int, call int64) error {
	class := fault.Classify(err)
	span.RecordError(err)
	code := fault.CodeOf(err)
	d.emit(ctx, Event{Type: EventRejected, Action: a.Name, Call: call, Scope: a.Scope, Trust: a.Trust.String(), Goal: a.Goal, At: d.clk.Now().UnixNano(), Err: string(class), Code: code})
	d.ob.Log.Warn(ctx, "dispatch rejected",
		observe.String("action", a.Name), observe.String("class", string(class)), observe.String("code", code))
	d.actions.Add(ctx, 1, observe.String("action", a.Name), observe.String("outcome", "rejected"))
	d.unwind(ctx, a, Metering{}, err, entered)
	return err
}

// unwind runs After for the first n hooks in reverse, like a defer stack.
func (d *Dispatcher) unwind(ctx context.Context, a Action, m Metering, err error, n int) {
	for i := n - 1; i >= 0; i-- {
		d.hooks[i].After(ctx, a, m, err)
	}
}

// emit appends an event. A sink error is logged and does not fail the action.
func (d *Dispatcher) emit(ctx context.Context, e Event) {
	if err := d.events.Append(ctx, e); err != nil {
		d.ob.Log.Warn(ctx, "event sink append failed",
			observe.String("event", e.Type), observe.String("action", e.Action))
	}
}

// AllowAll admits every action; the standalone default.
type AllowAll struct{}

// Admit implements Admitter.
func (AllowAll) Admit(context.Context, Action) error { return nil }

// DiscardSink drops events; the standalone default.
type DiscardSink struct{}

// Append implements EventSink.
func (DiscardSink) Append(context.Context, Event) error { return nil }

var (
	_ Admitter  = AllowAll{}
	_ EventSink = DiscardSink{}
)
