package reconcile

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

const taskAPIVersion = "test.ionagent.io/v1"
const taskKind = "Task"

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
