package instance

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

// DefaultHeartbeatInterval is how often a live process refreshes its Instance
// record. It is well under DefaultStaleAfter so a single missed beat never marks a
// healthy process stale: the staleness window spans several intervals, and only a
// process that has stopped writing entirely crosses it.
const DefaultHeartbeatInterval = 30 * time.Second

// Reporter observes what this process is doing right now and returns the run-state
// to record together with the ids of the runs driving it. It is injected so the
// instance package stays decoupled from the goal and run domains: the caller, which
// knows how to read its own work, supplies the derivation. A Reporter must be pure
// of side effects and quick; it is called on every beat. Returning StateUnknown is
// the honest answer when the caller cannot determine its own state.
type Reporter func(ctx context.Context) (State, []string)

// Heartbeat keeps a live process's Instance record current. It registers the
// process on start, writes its observed state once immediately, then on each
// interval re-observes and rewrites the record. Every write refreshes the heartbeat
// (the envelope's write time), so a record that stops moving is a process that has
// stopped, which the effective-state rule reports as Unknown rather than leaving
// frozen at its last live state. On a clean stop the process records a terminal
// Done, so a deliberate shutdown is distinguished from a crash.
//
// Heartbeat is the write half of the read surface: flynn ps/status, the dashboard,
// and the remote API all read the records it maintains. Without it those views show
// a process as perpetually Idle with a heartbeat that only advances when observed,
// which is exactly backwards.
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

// WithInterval sets the beat interval. A non-positive interval is ignored, leaving
// the default, so a misconfiguration never produces a busy loop.
func WithInterval(d time.Duration) Option {
	return func(h *Heartbeat) {
		if d > 0 {
			h.interval = d
		}
	}
}

// WithErrorHandler installs a callback for the transient store errors a beat may
// hit. A heartbeat must outlive a single failed write, so by default an error is
// swallowed and the next beat retries; a caller that wants to surface or count them
// supplies a handler. A nil handler restores the default (ignore).
func WithErrorHandler(fn func(error)) Option {
	return func(h *Heartbeat) { h.onError = fn }
}

// NewHeartbeat builds a Heartbeat for this process's Instance record. id is the
// stable instance id (the store's InstanceID), spec its declared shape, report the
// observer of its current work, and clk the time source (System in production, a
// Manual clock under test so the loop is deterministic and never sleeps).
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

// Run registers the instance, writes its state once so the record is correct from
// the start (never lingering on a stale state preserved from a previous process
// life), then beats on the interval until ctx is cancelled. On cancellation it
// records a terminal Done with a fresh context, so a cleanly stopped process reads
// Done rather than decaying to Unknown. Run returns the registration error if the
// initial Register fails (a process that cannot announce itself should surface it);
// per-beat errors go to the error handler and never stop the loop.
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

// beat observes the current state and writes it, refreshing the heartbeat. A store
// error is reported and otherwise ignored so the loop survives a transient failure.
func (h *Heartbeat) beat(ctx context.Context) {
	state, runs := h.report(ctx)
	if _, err := SetStatus(ctx, h.store, h.scope, h.id, state, runs); err != nil {
		h.fail(err)
	}
}

// shutdown records the terminal Done state on a clean stop. It uses a fresh,
// time-bounded context because the run context is already cancelled, so the final
// write still lands rather than being skipped.
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
