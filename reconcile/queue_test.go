package reconcile

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

func epoch() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestQueueDedup(t *testing.T) {
	q := NewQueue[string](clock.System{})
	q.Add("a")
	q.Add("a")
	q.Add("a")
	if q.Len() != 1 {
		t.Fatalf("three adds of one key -> Len %d, want 1", q.Len())
	}
	got, sd := q.Get()
	if sd || got != "a" {
		t.Fatalf("Get = (%q, %v)", got, sd)
	}
	q.Done("a")
	if q.Len() != 0 {
		t.Fatalf("after Done with no re-add, Len %d, want 0", q.Len())
	}
}

// TestQueueNeverConcurrentPerKey is the critical invariant, checked
// deterministically: while a key is being processed it is NOT available, and a
// re-add during processing is re-queued exactly once on Done.
func TestQueueNeverConcurrentPerKey(t *testing.T) {
	q := NewQueue[string](clock.System{})
	q.Add("a")

	got, _ := q.Get() // "a" now processing
	if got != "a" {
		t.Fatalf("Get = %q", got)
	}
	q.Add("a") // arrives while processing -> remembered, not re-queued yet
	if q.Len() != 0 {
		t.Fatalf("key available while still processing: Len %d, want 0", q.Len())
	}
	q.Done("a") // now it may be re-queued
	if q.Len() != 1 {
		t.Fatalf("re-add not re-queued on Done: Len %d, want 1", q.Len())
	}
	got2, _ := q.Get()
	if got2 != "a" {
		t.Fatalf("re-queued Get = %q, want a", got2)
	}
	q.Done("a")
	if q.Len() != 0 {
		t.Fatalf("final Len %d, want 0", q.Len())
	}
}

// TestQueueConcurrentStress drives many workers over a few hot keys and asserts no
// key is ever held by two workers at once (the per-key serialization guarantee).
func TestQueueConcurrentStress(t *testing.T) {
	q := NewQueue[int](clock.System{})
	const keys, workers = 4, 8
	var inFlight [keys]int32
	var violated int32

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				item, sd := q.Get()
				if sd {
					return
				}
				if atomic.AddInt32(&inFlight[item], 1) != 1 {
					atomic.StoreInt32(&violated, 1)
				}
				atomic.AddInt32(&inFlight[item], -1)
				q.Done(item)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		for i := range 200000 {
			q.Add(i % keys)
		}
		close(done)
	}()
	<-done
	time.Sleep(20 * time.Millisecond) // let workers drain
	q.ShutDown()
	wg.Wait()

	if atomic.LoadInt32(&violated) != 0 {
		t.Fatal("a key was processed by two workers concurrently")
	}
}

func TestQueueAddAfterDeterministic(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)

	q.AddAfter("x", 10*time.Second) // timer registered synchronously
	if q.Len() != 0 {
		t.Fatalf("AddAfter made the item ready early: Len %d", q.Len())
	}
	m.Advance(9 * time.Second)
	if q.Len() != 0 {
		t.Fatalf("item ready before its delay elapsed: Len %d", q.Len())
	}
	m.Advance(1 * time.Second) // reaches the deadline -> timer fires -> Add
	got, sd := q.Get()         // blocks until the firing goroutine Adds
	if sd || got != "x" {
		t.Fatalf("Get after delay = (%q, %v), want x", got, sd)
	}
	q.ShutDown()
}

// TestQueueAddAfterManyDeterministic proves that many delayed adds are each
// registered synchronously (so the Manual clock fires them deterministically) and
// all become ready once the clock passes their deadlines, no matter the order
// they were scheduled in.
func TestQueueAddAfterManyDeterministic(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[int](m)
	const n = 100

	// Schedule out of deadline order to exercise the heap ordering.
	for i := range n {
		q.AddAfter(i, time.Duration(n-i)*time.Second)
	}
	// Every timer is registered before its AddAfter returned.
	if p := m.PendingTimers(); p != n {
		t.Fatalf("PendingTimers = %d, want %d (each AddAfter registers synchronously)", p, n)
	}
	if q.Len() != 0 {
		t.Fatalf("no item should be ready before its delay: Len %d", q.Len())
	}

	m.Advance(n * time.Second) // past every deadline
	seen := map[int]bool{}
	for range n {
		got, sd := q.Get() // blocks until the waiting loop has enqueued each
		if sd {
			t.Fatal("unexpected shutdown while draining")
		}
		if seen[got] {
			t.Fatalf("item %d delivered twice", got)
		}
		seen[got] = true
	}
	if len(seen) != n {
		t.Fatalf("drained %d distinct items, want %d", len(seen), n)
	}
	q.ShutDown()
}

// TestQueueAddAfterSingleGoroutine is the point of the delaying queue: a storm of
// delayed adds must be serviced by one waiting goroutine, not one per item.
func TestQueueAddAfterSingleGoroutine(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[int](m)

	runtime.GC()
	before := runtime.NumGoroutine()
	const storm = 500
	for i := range storm {
		q.AddAfter(i, time.Hour) // long delay: none fire during the test
	}
	// Each AddAfter registers its timer before returning, so every delayed item
	// is already accounted for without waiting on the loop.
	if p := m.PendingTimers(); p != storm {
		t.Fatalf("PendingTimers = %d, want %d", p, storm)
	}
	time.Sleep(5 * time.Millisecond) // let the one waiting loop get scheduled
	added := runtime.NumGoroutine() - before
	if added > 16 {
		t.Fatalf("%d delayed adds spawned %d goroutines; want O(1) (a single waiting loop)", storm, added)
	}
	if q.Len() != 0 {
		t.Fatalf("no delayed item should be ready yet: Len %d", q.Len())
	}
	q.ShutDown()
}

func TestQueueRateLimitedBackoff(t *testing.T) {
	m := clock.NewManual(epoch())
	q := NewQueue[string](m)

	q.AddRateLimited("x")
	if n := q.NumRequeues("x"); n != 1 {
		t.Fatalf("NumRequeues after one AddRateLimited = %d, want 1", n)
	}
	m.Advance(defaultBaseDelay) // first backoff is exactly baseDelay
	if got, _ := q.Get(); got != "x" {
		t.Fatalf("rate-limited item not ready after baseDelay: %q", got)
	}
	q.Done("x")

	// Backoff grows: the second attempt needs more than baseDelay.
	q.AddRateLimited("x")
	q.AddRateLimited("x")
	if n := q.NumRequeues("x"); n != 3 {
		t.Fatalf("NumRequeues = %d, want 3", n)
	}
	q.Forget("x")
	if n := q.NumRequeues("x"); n != 0 {
		t.Fatalf("after Forget NumRequeues = %d, want 0", n)
	}
	q.ShutDown()
}

func TestQueueBackoffGrowsAndCaps(t *testing.T) {
	q := NewQueue[string](clock.System{})
	// First few delays double from baseDelay; eventually capped at maxDelay.
	d0 := q.backoff("k")
	d1 := q.backoff("k")
	d2 := q.backoff("k")
	if d0 != defaultBaseDelay || d1 != 2*defaultBaseDelay || d2 != 4*defaultBaseDelay {
		t.Fatalf("backoff sequence = %v,%v,%v, want base,2x,4x", d0, d1, d2)
	}
	for range 60 {
		_ = q.backoff("k")
	}
	if d := q.backoff("k"); d != defaultMaxDelay {
		t.Fatalf("backoff did not cap at maxDelay: %v", d)
	}
}

// TestQueueAddAfterRaceWithShutDown guards a WaitGroup ordering invariant:
// AddAfter registers its timer goroutine on the same WaitGroup that ShutDown
// waits on, so the two must never run wg.Add concurrently with wg.Wait. Without
// the lock-gated registration, AddAfter could pass its shutdown check, ShutDown
// could then close out with the counter at zero so Wait returned, and the late
// wg.Add would panic with "WaitGroup is reused before previous Wait has
// returned". Many iterations of AddAfter racing a concurrent ShutDown make the
// interleaving reliable; run under -race to also flag the unsynchronized access.
func TestQueueAddAfterRaceWithShutDown(t *testing.T) {
	guard := func(op string, fn func()) func() {
		return func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked racing the other: %v", op, r)
				}
			}()
			fn()
		}
	}
	for range 2000 {
		q := NewQueue[string](clock.System{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			guard("AddAfter", func() { q.AddAfter("a", 5*time.Millisecond) })()
		}()
		go func() {
			defer wg.Done()
			guard("ShutDown", q.ShutDown)()
		}()
		wg.Wait()
	}
}

func TestQueueShutDown(t *testing.T) {
	q := NewQueue[string](clock.System{})
	q.ShutDown()
	if _, sd := q.Get(); !sd {
		t.Fatal("Get after ShutDown should report shutdown")
	}
	q.ShutDown() // idempotent
}
