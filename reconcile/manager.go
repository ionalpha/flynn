package reconcile

import (
	"context"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

// Ref identifies the resource a reconcile acts on: kind + scope + name, the
// logical key of the resource store. It is the only thing a Reconciler is handed,
// so the reconciler must re-read the live resource itself (level-triggered).
type Ref = resource.Key

// DefaultResync is how often the manager re-enqueues every live resource of each
// registered kind when no interval is configured. Resync is the safety net: even
// if a change hint is lost (the bus is at-most-once), every resource is reconciled
// again within this window, so the system always converges.
const DefaultResync = 30 * time.Second

// Manager wires reconcilers to the resource store. It owns one Controller per
// registered kind, routes change hints to the right controller's queue, and runs
// the periodic resync that guarantees eventual convergence. It deliberately holds
// no watch of its own: truth is the store, and the manager only decides WHEN to
// reconcile, never WHAT the state is.
type Manager struct {
	store       resource.Store
	clk         clock.Timing
	resync      time.Duration
	controllers map[string]*Controller[Ref]
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithClock sets the time source (default clock.System). A Manual clock makes the
// resync ticker deterministic in tests.
func WithClock(c clock.Timing) ManagerOption {
	return func(m *Manager) {
		if c != nil {
			m.clk = c
		}
	}
}

// WithResync sets the resync interval (default DefaultResync). A value <= 0
// disables periodic resync (only the initial resync and explicit Enqueue drive
// reconciles), which is useful in tests that want to control every trigger.
func WithResync(d time.Duration) ManagerOption {
	return func(m *Manager) { m.resync = d }
}

// NewManager returns a manager over store. Register kinds before Start.
func NewManager(store resource.Store, opts ...ManagerOption) *Manager {
	m := &Manager{
		store:       store,
		clk:         clock.System{},
		resync:      DefaultResync,
		controllers: map[string]*Controller[Ref]{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Register attaches a reconciler for a kind. It must be called before Start.
// Registering the same kind twice replaces the earlier reconciler.
func (m *Manager) Register(kind string, rec Reconciler[Ref], opts ...ControllerOption) {
	q := NewQueue[Ref](m.clk)
	m.controllers[kind] = NewController(kind, q, rec, opts...)
}

// Enqueue schedules a reconcile of ref. This is the change-hint entry point: a
// bus signal, a write callback, or any "this resource may have changed" notice
// calls it. Hints are advisory (the reconcile re-reads truth), and a hint for an
// unregistered kind is ignored.
func (m *Manager) Enqueue(ref Ref) {
	if c, ok := m.controllers[ref.Kind]; ok {
		c.Queue().Add(ref)
	}
}

// Start runs every controller and the resync loop, blocking until ctx is
// cancelled and all of them have drained. Run it in its own goroutine; cancel ctx
// to stop.
func (m *Manager) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.controllers {

		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.resyncLoop(ctx)
	}()
	wg.Wait()
}

// resyncLoop performs an initial resync, then re-syncs every interval until ctx
// is cancelled.
func (m *Manager) resyncLoop(ctx context.Context) {
	m.resyncAll(ctx)
	if m.resync <= 0 {
		<-ctx.Done()
		return
	}
	for {
		t := m.clk.NewTimer(m.resync)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
			m.resyncAll(ctx)
		}
	}
}

// resyncAll re-enqueues every live resource of each registered kind. List errors
// are skipped: the next resync retries, and Enqueue hints still flow.
func (m *Manager) resyncAll(ctx context.Context) {
	for kind, c := range m.controllers {
		resources, err := m.store.ListAll(ctx, kind, nil)
		if err != nil {
			continue
		}
		for _, r := range resources {
			c.Queue().Add(r.Key())
		}
	}
}
