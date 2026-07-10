package procs

import (
	"sync"
	"testing"
)

// TestLiveCountsStartedMinusReaped is the counter's whole contract.
func TestLiveCountsStartedMinusReaped(t *testing.T) {
	var r Registry
	if got := r.Live(); got != 0 {
		t.Fatalf("zero Registry reports %d live, want 0", got)
	}

	a, b := r.Started(), r.Started()
	if got := r.Live(); got != 2 {
		t.Fatalf("after two spawns: %d live, want 2", got)
	}
	a()
	if got := r.Live(); got != 1 {
		t.Fatalf("after one reap: %d live, want 1", got)
	}
	b()
	if got := r.Live(); got != 0 {
		t.Fatalf("after both reaps: %d live, want 0", got)
	}
}

// TestReapedIsIdempotent pins the property the spawners depend on. A spawn path that
// releases on an error path and again from its reap goroutine must not double-decrement:
// the count is read by a leak detector, and a count driven below zero would make an
// unreaped child look like a reaped one for as long as the error persisted.
func TestReapedIsIdempotent(t *testing.T) {
	var r Registry
	reaped := r.Started()
	for i := range 5 {
		reaped()
		if got := r.Live(); got != 0 {
			t.Fatalf("after %d reap calls: %d live, want 0", i+1, got)
		}
	}
}

// TestReapedIsIdempotentUnderConcurrency: the reap function is called from whichever
// goroutine owns the child's exit, and an error path may race it. Exactly one of the
// racing calls may decrement.
func TestReapedIsIdempotentUnderConcurrency(t *testing.T) {
	var r Registry
	reaped := r.Started()

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); reaped() }()
	}
	wg.Wait()

	if got := r.Live(); got != 0 {
		t.Fatalf("16 racing reaps of one child: %d live, want 0", got)
	}
}

// TestConcurrentSpawnAndReap: the spawners run on many goroutines and the sampler reads
// while they do. Under -race this also pins that Live is safe to call concurrently with
// Started and with a reap.
func TestConcurrentSpawnAndReap(t *testing.T) {
	var r Registry

	const workers, each = 8, 200
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				reaped := r.Started()
				_ = r.Live() // a sampler reading mid-flight
				reaped()
			}
		}()
	}
	wg.Wait()

	if got := r.Live(); got != 0 {
		t.Fatalf("after %d balanced spawn/reap pairs: %d live, want 0", workers*each, got)
	}
}

// TestRegistriesAreIndependent: the package-level functions read one registry, but a
// Registry value is self-contained, so an embedder or a test can hold its own.
func TestRegistriesAreIndependent(t *testing.T) {
	var a, b Registry
	reaped := a.Started()
	defer reaped()

	if got := b.Live(); got != 0 {
		t.Fatalf("second registry saw %d live from the first, want 0", got)
	}
}

// TestPackageLevelTracksStd: the exported functions are the std registry's methods, which
// is what cmd/flynn hands to diag as Config.Children.
func TestPackageLevelTracksStd(t *testing.T) {
	before := Live()
	reaped := Started()
	if got := Live(); got != before+1 {
		t.Fatalf("package Live is %d after a spawn, want %d", got, before+1)
	}
	reaped()
	if got := Live(); got != before {
		t.Fatalf("package Live is %d after the reap, want %d", got, before)
	}
}
