package instance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

func hbStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

func reportState(state State, runs ...string) Reporter {
	return func(context.Context) (State, []string) { return state, runs }
}

// waitFor polls cond up to ~2s without consulting the wall clock (time.Now is
// banned for determinism); it only awaits the goroutine-driven loop's writes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func decodeState(t *testing.T, store resource.Store, id string) (State, []string) {
	t.Helper()
	r, err := store.Get(context.Background(), Kind, resource.Scope{}, id)
	if err != nil {
		t.Fatalf("get %q: %v", id, err)
	}
	st, err := DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st.State, st.Runs
}

func TestHeartbeatBeatRecordsReportedState(t *testing.T) {
	ctx := context.Background()
	store := hbStore(t)
	if _, err := Register(ctx, store, resource.Scope{}, "node-a", Spec{Version: "v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := NewHeartbeat(store, resource.Scope{}, "node-a", Spec{Version: "v1"},
		reportState(StateWorking, "run-1", "run-2"), clock.System{})
	h.beat(ctx)

	state, runs := decodeState(t, store, "node-a")
	if state != StateWorking {
		t.Fatalf("state = %q, want Working", state)
	}
	if len(runs) != 2 || runs[0] != "run-1" || runs[1] != "run-2" {
		t.Fatalf("runs = %v, want [run-1 run-2]", runs)
	}
}

func TestHeartbeatShutdownRecordsDone(t *testing.T) {
	ctx := context.Background()
	store := hbStore(t)
	if _, err := Register(ctx, store, resource.Scope{}, "node-a", Spec{Version: "v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A live run is recorded first; a clean shutdown must clear it and report Done.
	if _, err := SetStatus(ctx, store, resource.Scope{}, "node-a", StateWorking, []string{"run-1"}); err != nil {
		t.Fatalf("set status: %v", err)
	}

	h := NewHeartbeat(store, resource.Scope{}, "node-a", Spec{Version: "v1"},
		reportState(StateWorking, "run-1"), clock.System{})
	h.shutdown()

	state, runs := decodeState(t, store, "node-a")
	if state != StateDone {
		t.Fatalf("state = %q, want Done", state)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %v, want empty after shutdown", runs)
	}
}

// faultyPut wraps a store and fails every Put once armed, so a beat's write error
// can be observed without depending on store internals.
type faultyPut struct {
	resource.Store
	fail bool
}

func (f *faultyPut) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if f.fail {
		return resource.Resource{}, errors.New("write failed")
	}
	return f.Store.Put(ctx, r)
}

func TestHeartbeatBeatSurfacesStoreError(t *testing.T) {
	ctx := context.Background()
	base := hbStore(t)
	if _, err := Register(ctx, base, resource.Scope{}, "node-a", Spec{Version: "v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	faulty := &faultyPut{Store: base, fail: true}

	var got error
	h := NewHeartbeat(faulty, resource.Scope{}, "node-a", Spec{Version: "v1"},
		reportState(StateWorking, "run-1"), clock.System{},
		WithErrorHandler(func(err error) { got = err }))
	h.beat(ctx) // must not panic; the loop survives a failed write.

	if got == nil {
		t.Fatal("expected the error handler to receive the failed write")
	}
}

func TestHeartbeatRunLifecycle(t *testing.T) {
	store := hbStore(t)
	clk := clock.NewManual(time.Unix(0, 0).UTC())

	// The reporter's answer is swapped mid-run to prove a later beat picks up the
	// change rather than freezing at the first observation. A one-slot channel guards
	// the current reporter so the loop goroutine and the test never race on it.
	reporterMu := make(chan Reporter, 1)
	reporterMu <- reportState(StateIdle)
	dynamic := func(ctx context.Context) (State, []string) {
		r := <-reporterMu
		reporterMu <- r
		return r(ctx)
	}
	swap := func(r Reporter) { <-reporterMu; reporterMu <- r }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	h := NewHeartbeat(store, resource.Scope{}, "node-a", Spec{Host: "h", Version: "v1"},
		dynamic, clk, WithInterval(30*time.Second))
	go func() { done <- h.Run(ctx) }()

	// The initial beat lands immediately, before any clock advance: the record is
	// correct from the start, not on the next tick.
	waitFor(t, func() bool {
		r, err := store.Get(context.Background(), Kind, resource.Scope{}, "node-a")
		return err == nil && r.SyncVersion > 0
	})
	if state, _ := decodeState(t, store, "node-a"); state != StateIdle {
		t.Fatalf("initial state = %q, want Idle", state)
	}

	// Once the loop arms its interval timer, advancing past it drives exactly one
	// beat, which observes the now-changed state.
	swap(reportState(StateWorking, "run-1"))
	waitFor(t, func() bool { return clk.PendingTimers() == 1 })
	before, _ := store.Get(context.Background(), Kind, resource.Scope{}, "node-a")
	clk.Advance(30 * time.Second)
	waitFor(t, func() bool {
		r, err := store.Get(context.Background(), Kind, resource.Scope{}, "node-a")
		return err == nil && r.SyncVersion > before.SyncVersion
	})
	if state, runs := decodeState(t, store, "node-a"); state != StateWorking || len(runs) != 1 {
		t.Fatalf("after beat: state=%q runs=%v, want Working [run-1]", state, runs)
	}

	// A clean stop records the terminal Done.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if state, _ := decodeState(t, store, "node-a"); state != StateDone {
		t.Fatalf("after shutdown: state=%q, want Done", state)
	}
}
