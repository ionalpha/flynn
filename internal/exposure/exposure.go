// Package exposure records everything Flynn has open to the network: what is
// listening, why, since when and until when. bindguard decides whether a listener
// may bind and netguard where the agent may connect; exposure tracks what is open
// and tears it down when its lease ends.
//
// Nothing stays exposed unseen: every listener opened through it is listed (List),
// logged on open and close, and with a TTL closed when the lease expires instead of
// lingering until the process dies. An ephemeral exposure such as a preview server
// is bounded; a long-lived one such as the control-plane API is always visible.
package exposure

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/observe"
)

// Meta says why something is exposed, for the audit record and the teardown lease.
type Meta struct {
	// Purpose is a short label for the audit trail ("control-plane API").
	Purpose string
	// Exposed reports whether the bind reached beyond loopback, an audited
	// off-host exposure.
	Exposed bool
	// TTL, when positive, is how long the exposure lives before automatic teardown.
	// Zero means it lives until closed.
	TTL time.Duration
}

// Record is a snapshot of one live exposure, as returned by List.
type Record struct {
	ID       string
	Addr     string
	Purpose  string
	Exposed  bool
	OpenedAt time.Time
	// ExpiresAt is the lease deadline, or the zero time when there is no TTL.
	ExpiresAt time.Time
}

type entry struct {
	rec   Record
	ln    net.Listener
	timer clock.Timer
	// done is closed on teardown so the lease goroutine exits when the exposure
	// closes before its timer fires, since a stopped System timer never delivers.
	done chan struct{}
}

// Registry tracks live network exposures and enforces their leases. Build one with
// New; the zero value is not usable. It is safe for concurrent use.
type Registry struct {
	clk clock.Timing
	obs observe.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

// New returns a Registry scheduling TTL teardown on clk and logging to obs. A nil
// clk uses the system clock; a nil obs discards.
func New(clk clock.Timing, obs observe.Logger) *Registry {
	if clk == nil {
		clk = clock.System{}
	}
	if obs == nil {
		obs = observe.Default().Log
	}
	return &Registry{clk: clk, obs: obs, entries: make(map[string]*entry)}
}

// Listen opens a listener through bindguard and registers it, so every exposure
// passes one recording point. Closing the returned listener deregisters it, and a
// positive meta.TTL closes it when the lease expires.
func (r *Registry) Listen(network, addr string, exp bindguard.Exposure, meta Meta) (net.Listener, error) {
	ln, err := bindguard.Listen(network, addr, exp)
	if err != nil {
		return nil, err
	}
	return r.track(ln, meta), nil
}

// track registers an already-open listener and returns a wrapper whose Close
// deregisters it. Prefer Listen, which sends the bind through the gate.
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

// awaitExpiry closes the exposure when its lease fires, or returns when the
// exposure was already torn down, so a stopped timer never leaks the goroutine.
func (r *Registry) awaitExpiry(e *entry) {
	select {
	case <-e.timer.C():
		r.teardown(e.rec.ID, "expired")
	case <-e.done:
	}
}

// teardown removes the entry and closes its listener once, logging the reason. A
// second teardown for the same id (lease then Close, or the reverse) is a no-op.
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

// List returns the live exposures sorted by open time, so an operator or the
// control plane sees what is reachable.
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

// trackedListener deregisters the exposure on Close, so a closed listener never
// leaves a stale record.
type trackedListener struct {
	net.Listener
	reg *Registry
	id  string
}

// Close deregisters the exposure and closes the listener.
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
