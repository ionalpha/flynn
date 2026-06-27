// Package exposure is the registry of everything Flynn has open to the network: the
// record that makes the inbound-exposure boundary observable and time-bounded. Where
// bindguard decides whether a listener may bind (the gate) and netguard decides where
// the agent may connect (the egress gate), exposure tracks what is currently listening,
// why, since when, and until when, and tears it down when its lease ends.
//
// The invariant it enforces is "nothing stays exposed silently": every listener opened
// through it is recorded and enumerable (List), logged on open and on close, and, when
// given a TTL, closed automatically when the lease expires rather than lingering until a
// process dies. An ephemeral exposure (a preview server, a temporary tunnel) is bounded
// by construction; a long-lived one (the control-plane API) is at least always visible.
//
// The registry is the in-process record today; a durable, control-plane-visible
// resource kind is the natural upgrade once managed services land, and this is the
// mechanism it would write through.
package exposure

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bindguard"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/observe"
)

// Meta describes why something is exposed, for the audit record and the teardown lease.
type Meta struct {
	// Purpose is a short human label for the audit trail, e.g. "control-plane API".
	Purpose string
	// Exposed reports whether the bind reached beyond loopback (an explicit, audited
	// off-host exposure) rather than the loopback default.
	Exposed bool
	// TTL, when positive, is how long the exposure may live before the registry tears
	// it down automatically. Zero means no lease: it lives until closed explicitly.
	TTL time.Duration
}

// Record is a snapshot of one live exposure, returned by List for observation.
type Record struct {
	ID       string
	Addr     string
	Purpose  string
	Exposed  bool
	OpenedAt time.Time
	// ExpiresAt is the lease deadline, or the zero time when there is no TTL.
	ExpiresAt time.Time
}

// entry is the registry's live bookkeeping for one exposure.
type entry struct {
	rec   Record
	ln    net.Listener
	timer clock.Timer
	// done is closed on teardown so the lease goroutine exits even when the exposure is
	// closed before its timer fires (a stopped System timer never delivers on C).
	done chan struct{}
}

// Registry tracks live network exposures and enforces their leases. The zero value is
// not usable; build one with New. It is safe for concurrent use.
type Registry struct {
	clk clock.Timing
	obs observe.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

// New returns a Registry that schedules TTL teardown on clk and logs lifecycle events
// to obs. A nil clk uses the system clock; a nil obs discards.
func New(clk clock.Timing, obs observe.Logger) *Registry {
	if clk == nil {
		clk = clock.System{}
	}
	if obs == nil {
		obs = observe.Default().Log
	}
	return &Registry{clk: clk, obs: obs, entries: make(map[string]*entry)}
}

// Listen opens a listener through the bind-safe gate (bindguard) and registers it, so
// every exposure flows through one chokepoint that records it. The returned listener is
// wrapped: closing it deregisters the exposure. If meta.TTL is positive, the registry
// closes the listener when the lease expires.
func (r *Registry) Listen(network, addr string, exp bindguard.Exposure, meta Meta) (net.Listener, error) {
	ln, err := bindguard.Listen(network, addr, exp)
	if err != nil {
		return nil, err
	}
	return r.track(ln, meta), nil
}

// track records an already-open listener and returns a wrapper whose Close deregisters
// it. It is the seam for a listener opened outside Listen (a test, or a caller that must
// build the listener itself); prefer Listen so the bind goes through the gate.
func (r *Registry) track(ln net.Listener, meta Meta) net.Listener {
	now := r.clk.Now()
	e := &entry{
		rec: Record{
			ID:       ids.New(),
			Addr:     ln.Addr().String(),
			Purpose:  meta.Purpose,
			Exposed:  meta.Exposed,
			OpenedAt: now,
		},
		ln:   ln,
		done: make(chan struct{}),
	}
	if meta.TTL > 0 {
		e.rec.ExpiresAt = now.Add(meta.TTL)
	}

	r.mu.Lock()
	r.entries[e.rec.ID] = e
	r.mu.Unlock()

	r.obs.Info(context.Background(), "exposure: opened",
		observe.String("id", e.rec.ID), observe.String("addr", e.rec.Addr),
		observe.String("purpose", e.rec.Purpose), exposedField(e.rec.Exposed))

	if meta.TTL > 0 {
		e.timer = r.clk.NewTimer(meta.TTL)
		go r.awaitExpiry(e)
	}
	return &trackedListener{Listener: ln, reg: r, id: e.rec.ID}
}

// awaitExpiry closes the exposure when its lease timer fires, or exits quietly if the
// exposure was already torn down (done closed), so a stopped timer never leaks the
// goroutine.
func (r *Registry) awaitExpiry(e *entry) {
	select {
	case <-e.timer.C():
		r.teardown(e.rec.ID, "expired")
	case <-e.done:
	}
}

// teardown removes the entry and closes its listener exactly once, logging the reason.
// A second teardown for the same id (lease fired then Close, or vice versa) is a no-op.
func (r *Registry) teardown(id, reason string) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if ok {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	close(e.done) // release the lease goroutine if it is still waiting
	if e.timer != nil {
		e.timer.Stop()
	}
	_ = e.ln.Close()
	r.obs.Info(context.Background(), "exposure: closed",
		observe.String("id", id), observe.String("addr", e.rec.Addr),
		observe.String("purpose", e.rec.Purpose), observe.String("reason", reason))
}

// List returns a snapshot of the live exposures, sorted by open time, so an operator (or
// the control plane) can see exactly what is currently reachable.
func (r *Registry) List() []Record {
	r.mu.Lock()
	out := make([]Record, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.rec)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out
}

// trackedListener is a net.Listener whose Close also deregisters the exposure, so a
// caller closing its listener never leaves a stale record behind.
type trackedListener struct {
	net.Listener
	reg *Registry
	id  string
}

// Close deregisters the exposure and closes the underlying listener. It is the explicit
// (non-lease) teardown path.
func (t *trackedListener) Close() error {
	t.reg.teardown(t.id, "closed")
	return nil
}

func exposedField(exposed bool) observe.Field {
	if exposed {
		return observe.String("reach", "non-loopback")
	}
	return observe.String("reach", "loopback")
}
