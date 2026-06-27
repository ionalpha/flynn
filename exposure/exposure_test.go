package exposure

import (
	"net"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/bindguard"
	"github.com/ionalpha/flynn/clock"
)

func TestListenRegistersAndDeregistersOnClose(t *testing.T) {
	r := New(clock.System{}, nil)
	ln, err := r.Listen("tcp", "127.0.0.1:0", bindguard.Loopback(), Meta{Purpose: "test"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	recs := r.List()
	if len(recs) != 1 {
		t.Fatalf("List = %d entries, want 1", len(recs))
	}
	if recs[0].Purpose != "test" || recs[0].Addr != ln.Addr().String() {
		t.Errorf("record = %+v, want purpose=test addr=%s", recs[0], ln.Addr())
	}
	if !recs[0].ExpiresAt.IsZero() {
		t.Error("a no-TTL exposure must have a zero ExpiresAt")
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("after close, List = %d, want 0", got)
	}
}

func TestListenInheritsBindguardRefusal(t *testing.T) {
	r := New(clock.System{}, nil)
	// A wildcard bind is refused by the gate, so nothing is registered.
	if _, err := r.Listen("tcp", "0.0.0.0:0", bindguard.Loopback(), Meta{Purpose: "x"}); err == nil {
		t.Fatal("a wildcard bind should be refused")
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("a refused bind must not register; List = %d", got)
	}
}

func TestTTLTearsDownOnExpiry(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	r := New(clk, nil)
	ln, err := r.Listen("tcp", "127.0.0.1:0", bindguard.Loopback(), Meta{Purpose: "ephemeral", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if got := len(r.List()); got != 1 {
		t.Fatalf("before expiry List = %d, want 1", got)
	}
	// Advance past the lease; the timer fires and teardown runs asynchronously.
	clk.Advance(2 * time.Minute)
	if !waitFor(func() bool { return len(r.List()) == 0 }) {
		t.Fatal("exposure was not torn down after its TTL expired")
	}
	// The underlying listener is closed: accept fails immediately on a closed listener.
	if _, err := ln.Accept(); err == nil {
		t.Error("listener should be closed after TTL teardown")
	}
}

// waitFor polls cond up to ~2s without consulting the wall clock (time.Now is banned
// for determinism); it is only here to await an async teardown goroutine.
func waitFor(cond func() bool) bool {
	for range 2000 {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestCloseBeforeTTLDoesNotDoubleTeardown(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	r := New(clk, nil)
	ln, err := r.Listen("tcp", "127.0.0.1:0", bindguard.Loopback(), Meta{Purpose: "x", TTL: time.Minute})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(r.List()); got != 0 {
		t.Fatalf("after close List = %d, want 0", got)
	}
	// Firing the lease after an explicit close is a harmless no-op.
	clk.Advance(2 * time.Minute)
	if got := len(r.List()); got != 0 {
		t.Errorf("lease fire after close changed state; List = %d, want 0", got)
	}
}

func TestExposedFlagRecorded(t *testing.T) {
	r := New(clock.System{}, nil)
	ln, err := r.Listen("tcp", "127.0.0.1:0", bindguard.Loopback(), Meta{Purpose: "p", Exposed: true})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if !r.List()[0].Exposed {
		t.Error("Exposed flag should be recorded on the entry")
	}
}

// Property: across any interleaving of opens and closes, the registry's live count
// equals opens-minus-closes, never leaks or double-counts, and closing the same
// exposure twice is safe.
func TestRegistryBalanceProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := New(clock.System{}, nil)
		var open []net.Listener
		ops := rapid.IntRange(1, 40).Draw(t, "ops")
		for range ops {
			if len(open) > 0 && rapid.Bool().Draw(t, "close") {
				i := rapid.IntRange(0, len(open)-1).Draw(t, "which")
				ln := open[i]
				open = append(open[:i], open[i+1:]...)
				if err := ln.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				// Closing again must be safe (no panic, no negative count).
				_ = ln.Close()
			} else {
				ln, err := r.Listen("tcp", "127.0.0.1:0", bindguard.Loopback(), Meta{Purpose: "p"})
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				open = append(open, ln)
			}
			if got := len(r.List()); got != len(open) {
				t.Fatalf("live count = %d, want %d", got, len(open))
			}
		}
		for _, ln := range open {
			_ = ln.Close()
		}
		if got := len(r.List()); got != 0 {
			t.Fatalf("after closing all, live count = %d, want 0", got)
		}
	})
}
