package instance

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

// DefaultHeartbeatInterval is how often a live process refreshes its record. It
// is a fraction of [DefaultStaleAfter], so one missed beat never marks a healthy
// process stale.
const DefaultHeartbeatInterval = 30 * time.Second

// Reporter returns the run-state to record and the ids of the runs driving it.
// It is injected so this package does not depend on the goal and run domains.
// It runs on every beat, so it must be quick and free of side effects; it
// returns StateUnknown when the caller cannot tell.
type Reporter func(ctx context.Context) (State, []string)

// Heartbeat keeps a live process's Instance record current, so a record that
// stops moving means a stopped process and [EffectiveState] reports Unknown. A
// clean stop records Done, which tells a shutdown from a crash.
type Heartbeat struct {
	store    resource.Store
	scope    resource.Scope
	id       string
	spec     Spec
	clk      clock.Timing
	interval time.Duration
	report   Reporter
	onError  func(error)
}

// Option configures a Heartbeat.
type Option func(*Heartbeat)

// WithInterval sets the beat interval. A non-positive d is ignored so a
// misconfiguration cannot busy-loop.
func WithInterval(d time.Duration) Option {
	return func(h *Heartbeat) {
		if d > 0 {
			h.interval = d
		}
	}
}

// WithErrorHandler receives the store errors a beat hits. By default, and with
// a nil fn, they are dropped and the next beat retries.
func WithErrorHandler(fn func(error)) Option {
	return func(h *Heartbeat) { h.onError = fn }
}

// NewHeartbeat builds a Heartbeat for the record named id, the store's
// InstanceID. Tests pass a clock.Manual so the loop never sleeps.
func NewHeartbeat(store resource.Store, scope resource.Scope, id string, spec Spec, report Reporter, clk clock.Timing, opts ...Option) *Heartbeat {
	h := &Heartbeat{
		store:    store,
		scope:    scope,
		id:       id,
		spec:     spec,
		clk:      clk,
		interval: DefaultHeartbeatInterval,
		report:   report,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Run registers the instance and beats at once, so no state from a previous
// process life lingers, then beats on the interval until ctx is cancelled and
// records Done. It returns only a Register error; beat errors go to the error
// handler and never stop the loop.
func (h *Heartbeat) Run(ctx context.Context) error {
	if _, err := Register(ctx, h.store, h.scope, h.id, h.spec); err != nil {
		return err
	}
	h.beat(ctx)
	for {
		t := h.clk.NewTimer(h.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			h.shutdown()
			return nil
		case <-t.C():
			h.beat(ctx)
		}
	}
}

func (h *Heartbeat) beat(ctx context.Context) {
	state, runs := h.report(ctx)
	if _, err := SetStatus(ctx, h.store, h.scope, h.id, state, runs); err != nil {
		h.fail(err)
	}
}

// shutdown records Done on a fresh context, because the run context is already
// cancelled.
func (h *Heartbeat) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := SetStatus(ctx, h.store, h.scope, h.id, StateDone, nil); err != nil {
		h.fail(err)
	}
}

func (h *Heartbeat) fail(err error) {
	if h.onError != nil {
		h.onError(err)
	}
}
