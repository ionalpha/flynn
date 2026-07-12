package reconcile

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

const (
	taskAPIVersion = "test.ionagent.io/v1"
	taskKind       = "Task"
)

func newStore(t *testing.T, clk clock.Clock) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(resource.Kind{
		APIVersion: taskAPIVersion,
		Name:       taskKind,
		Schema:     json.RawMessage(`{"type":"object"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg, resource.WithClock(clk))
}

func putTask(t *testing.T, s resource.Store, name string) {
	t.Helper()
	if _, err := s.Put(context.Background(), resource.Resource{
		APIVersion: taskAPIVersion, Kind: taskKind, Name: name, Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
}

// collect drains refs into a name set until it has `want` distinct names or times
// out.
func collect(t *testing.T, ch <-chan Ref, want int) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < want {
		select {
		case r := <-ch:
			got[r.Name] = true
		case <-deadline:
			t.Fatalf("timed out; got %v, want %d distinct", got, want)
		}
	}
	return got
}

func TestManagerInitialResyncReconcilesExisting(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)
	putTask(t, store, "a")
	putTask(t, store, "b")

	reconciled := make(chan Ref, 16)
	mgr := NewManager(store, WithClock(m), WithResync(0)) // only initial resync + Enqueue
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	got := collect(t, reconciled, 2)
	if !got["a"] || !got["b"] {
		t.Fatalf("initial resync reconciled %v, want a and b", got)
	}
}

// listOnlyStore hides the KeyLister capability of the wrapped store, so the
// manager's ListAll fallback stays covered even though both bundled backends
// offer key-only reads.
type listOnlyStore struct{ resource.Store }

// TestManagerResyncWithoutKeyLister proves resync converges through the ListAll
// fallback when the store offers no key-only read.
func TestManagerResyncWithoutKeyLister(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)
	putTask(t, store, "a")
	putTask(t, store, "b")

	reconciled := make(chan Ref, 16)
	mgr := NewManager(listOnlyStore{store}, WithClock(m), WithResync(0))
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	got := collect(t, reconciled, 2)
	if !got["a"] || !got["b"] {
		t.Fatalf("fallback resync reconciled %v, want a and b", got)
	}
}

// TestManagerResyncScopeIgnoresExisting is the controlled-resume guarantee: a
// scoped manager never adopts a resource it finds already in the store, so a
// one-shot run does not resume a goal an earlier run left behind (which would
// contaminate its event stream). Only what the scope names is driven.
func TestManagerResyncScopeIgnoresExisting(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)
	putTask(t, store, "leftover")

	reconciled := make(chan Ref, 16)
	scope := &scopeSet{}
	mgr := NewManager(store, WithClock(m), WithResyncScope(scope.list))
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	// The pre-existing, out-of-scope resource must NOT be adopted, not on the initial
	// sweep and not on a later tick either.
	waitPending(t, m, 1)
	m.Advance(DefaultResync)
	select {
	case got := <-reconciled:
		t.Fatalf("scoped resync adopted an out-of-scope resource: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	// But explicitly enqueued work is still driven.
	mgr.Enqueue(Ref{Kind: taskKind, Name: "submitted"})
	if got := <-reconciled; got.Name != "submitted" {
		t.Fatalf("Enqueue reconciled %q, want submitted", got.Name)
	}
}

// TestManagerResyncScopeRecoversLostHint is the convergence guarantee that scoping
// must not cost: the bus is at-most-once, so a change hint for a resource this
// process submitted can simply be dropped. Resync is the only thing that recovers
// from that, and it must still run for what is in scope. Without it, one lost hint
// parks the run forever.
func TestManagerResyncScopeRecoversLostHint(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)
	putTask(t, store, "leftover")
	putTask(t, store, "mine")

	reconciled := make(chan Ref, 16)
	scope := &scopeSet{}
	scope.add(Ref{Kind: taskKind, Name: "mine"}) // submitted by this process
	mgr := NewManager(store, WithClock(m), WithResyncScope(scope.list))
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	// The initial scoped sweep drives the submitted resource...
	if got := <-reconciled; got.Name != "mine" {
		t.Fatalf("scoped resync reconciled %q, want mine", got.Name)
	}
	// ...and the periodic one keeps driving it, which is what recovers a hint the bus
	// dropped. No Enqueue is called here: the tick alone must do it.
	waitPending(t, m, 1)
	m.Advance(DefaultResync)
	if got := <-reconciled; got.Name != "mine" {
		t.Fatalf("periodic scoped resync reconciled %q, want mine", got.Name)
	}
	// The out-of-scope leftover was never touched by either sweep.
	select {
	case got := <-reconciled:
		t.Fatalf("scoped resync adopted an out-of-scope resource: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// scopeSet is the resync scope a one-shot runtime keeps: the resources this process
// submitted itself.
type scopeSet struct {
	mu   sync.Mutex
	refs []Ref
}

func (s *scopeSet) add(r Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs = append(s.refs, r)
}

func (s *scopeSet) list() []Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ref(nil), s.refs...)
}

func TestManagerEnqueueTriggersReconcile(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)

	reconciled := make(chan Ref, 16)
	mgr := NewManager(store, WithClock(m), WithResync(0))
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	mgr.Enqueue(Ref{Kind: taskKind, Name: "x"})
	if got := <-reconciled; got.Name != "x" {
		t.Fatalf("Enqueue reconciled %q, want x", got.Name)
	}
	// A hint for an unregistered kind is silently ignored (no panic, no reconcile).
	mgr.Enqueue(Ref{Kind: "Nope", Name: "y"})
	select {
	case got := <-reconciled:
		t.Fatalf("reconciled unregistered kind: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestManagerResyncSelfHeals proves the safety net: a resource that appears
// WITHOUT a change hint (a dropped signal) is still reconciled at the next resync.
func TestManagerResyncSelfHeals(t *testing.T) {
	m := clock.NewManual(epoch())
	store := newStore(t, m)
	putTask(t, store, "a")

	reconciled := make(chan Ref, 16)
	mgr := NewManager(store, WithClock(m), WithResync(30*time.Second))
	mgr.Register(taskKind, ReconcilerFunc[Ref](func(_ context.Context, ref Ref) (Result, error) {
		reconciled <- ref
		return Result{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Start(ctx)

	if got := collect(t, reconciled, 1); !got["a"] {
		t.Fatalf("initial resync = %v, want a", got)
	}

	// New resource, deliberately NOT enqueued (simulating a lost change hint).
	putTask(t, store, "c")
	waitPending(t, m, 1) // the resync timer is armed
	m.Advance(30 * time.Second)

	got := collect(t, reconciled, 1)
	// "c" must show up purely from resync (a may also reappear; we only require c).
	deadline := time.After(2 * time.Second)
	for !got["c"] {
		select {
		case r := <-reconciled:
			got[r.Name] = true
		case <-deadline:
			t.Fatalf("resync did not reconcile c; saw %v", got)
		}
	}
}
