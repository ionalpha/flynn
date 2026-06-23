package hlc_test

import (
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
)

func TestNowIncrementsCounterWithinMillis(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))

	a, b, d := c.Now(), c.Now(), c.Now()
	if !a.Before(b) || !b.Before(d) {
		t.Fatalf("not strictly increasing: %v %v %v", a, b, d)
	}
	if a != (hlc.Time{Wall: 1000, Counter: 0}) || b.Counter != 1 || d.Counter != 2 {
		t.Fatalf("counters = %v %v %v", a, b, d)
	}
}

func TestNowAdvancesWithPhysicalAndResetsCounter(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))
	c.Now()
	clk.Advance(time.Millisecond)
	if got := c.Now(); got != (hlc.Time{Wall: 1001, Counter: 0}) {
		t.Fatalf("got %v, want 1001.0", got)
	}
}

func TestNowMonotonicUnderBackwardClock(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))
	first := c.Now()
	clk.Set(time.UnixMilli(500)) // wall clock jumps backward (skew/NTP)
	second := c.Now()
	if !first.Before(second) {
		t.Fatalf("clock went backward: %v then %v", first, second)
	}
	if second != (hlc.Time{Wall: 1000, Counter: 1}) {
		t.Fatalf("got %v, want 1000.1", second)
	}
}

func TestObserveAdvancesPastRemote(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))
	c.Now()

	remote := hlc.Time{Wall: 5000, Counter: 9}
	got := c.Observe(remote)
	if got != (hlc.Time{Wall: 5000, Counter: 10}) {
		t.Fatalf("Observe = %v, want 5000.10", got)
	}
	if !remote.Before(got) {
		t.Fatalf("did not advance past remote: %v vs %v", remote, got)
	}
	if next := c.Now(); !got.Before(next) {
		t.Fatalf("Now after Observe not monotonic: %v then %v", got, next)
	}
}

func TestCompareAndZero(t *testing.T) {
	var z hlc.Time
	if !z.IsZero() {
		t.Fatal("zero value should be IsZero")
	}
	a := hlc.Time{Wall: 1}
	b := hlc.Time{Wall: 1, Counter: 1}
	c := hlc.Time{Wall: 2}
	if a.Compare(b) != -1 || b.Compare(c) != -1 || c.Compare(a) != 1 || a.Compare(a) != 0 {
		t.Fatal("Compare ordering wrong")
	}
	if b.Compare(a) != 1 {
		t.Fatal("higher counter should compare greater")
	}
	if !z.Before(a) {
		t.Fatal("zero must precede a real timestamp")
	}
	if a.String() != "1.0" || b.String() != "1.1" {
		t.Fatalf("String() = %q / %q", a.String(), b.String())
	}
}

func TestDeterministicUnderInjectedClock(t *testing.T) {
	run := func() []hlc.Time {
		clk := clock.NewManual(time.UnixMilli(7000))
		c := hlc.NewClock(hlc.WithPhysical(clk))
		out := []hlc.Time{c.Now(), c.Now()}
		clk.Advance(time.Millisecond)
		return append(out, c.Now())
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %v vs %v", i, a[i], b[i])
		}
	}
}

// Observe when local, remote, and physical all share the same wall time: the
// counter is max(local, remote) + 1.
func TestObserveSameWallTakesMaxCounter(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))
	c.Now() // {1000, 0}
	got := c.Observe(hlc.Time{Wall: 1000, Counter: 5})
	if got != (hlc.Time{Wall: 1000, Counter: 6}) {
		t.Fatalf("Observe = %v, want 1000.6", got)
	}
}

// Observe when physical time is strictly ahead of both clocks: a fresh tick.
func TestObservePhysicalAhead(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1000))
	c := hlc.NewClock(hlc.WithPhysical(clk))
	c.Now()
	clk.Set(time.UnixMilli(9000))
	got := c.Observe(hlc.Time{Wall: 500, Counter: 3})
	if got != (hlc.Time{Wall: 9000, Counter: 0}) {
		t.Fatalf("Observe = %v, want 9000.0", got)
	}
}

func TestConcurrentNowProducesUniqueMonotonic(t *testing.T) {
	c := hlc.NewClock()
	const n = 200
	out := make([]hlc.Time, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); out[i] = c.Now() }(i)
	}
	wg.Wait()

	seen := make(map[hlc.Time]struct{}, n)
	for _, ts := range out {
		if _, dup := seen[ts]; dup {
			t.Fatalf("duplicate HLC under concurrency: %v", ts)
		}
		seen[ts] = struct{}{}
	}
}
