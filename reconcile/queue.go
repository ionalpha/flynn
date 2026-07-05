// Package reconcile is the agent's desired-state execution engine: a small,
// in-process control loop in the Kubernetes mould (a level-triggered reconciler
// driven by a deduplicating work queue) but with no cluster, no apiserver, and no
// etcd. It runs in one binary over the resource store, the event log, and the job
// queue. The control-theory mechanics are borrowed (they are domain-agnostic and
// proven); the engine on top, reconciling agent goals toward an LLM-judged stop
// condition, is the agent's own.
//
// This file is the critical primitive: the work queue. It mirrors the
// semantics of client-go's controller workqueue (a key is never processed
// concurrently; adds collapse while a key waits; delayed and rate-limited re-adds
// avoid hot loops) without importing it, and it schedules delays through a
// clock.Timing source so every delay is deterministic under a Manual clock.
package reconcile

import (
	"container/heap"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Default per-item exponential backoff bounds for AddRateLimited, matching the
// controller defaults (5ms base doubling to a 1000s ceiling).
const (
	defaultBaseDelay = 5 * time.Millisecond
	defaultMaxDelay  = 1000 * time.Second
)

// Queue is a deduplicating, level-triggered work queue keyed by T. A key added
// any number of times before it is fetched is processed once; while a key is
// being processed, re-adds are remembered and the key is re-queued exactly once
// when processing finishes (Done). AddAfter and AddRateLimited delay a re-add
// without blocking, so a failing item backs off instead of spinning. T is the
// item's identity (for the reconciler, a resource Ref), so the queue carries no
// payload: the reconciler always re-reads current state, never trusting a stale
// enqueued value.
type Queue[T comparable] struct {
	mu           sync.Mutex
	cond         *sync.Cond
	queue        []T            // ready items, in order; each is also in dirty
	dirty        map[T]struct{} // items that need processing
	processing   map[T]struct{} // items currently held by a worker
	shuttingDown bool

	clk         clock.Timing
	shutdownCh  chan struct{}
	closeOnce   sync.Once
	waitingCh   chan waitFor[T] // delayed adds handed to the single waiting loop
	loopStarted bool            // whether the waiting loop has been started (guarded by mu)
	loopDone    chan struct{}   // closed by the waiting loop when it exits

	rlMu      sync.Mutex
	failures  map[T]int
	baseDelay time.Duration
	maxDelay  time.Duration
}

// waitFor is a single delayed add carried from AddAfter to the waiting loop: the
// item to enqueue, the deadline it becomes ready, and the timer (created and
// registered on the clock synchronously in AddAfter) that wakes the loop.
type waitFor[T comparable] struct {
	item     T
	deadline time.Time
	timer    clock.Timer
}

// waitingBuffer sizes the channel from AddAfter to the waiting loop. It only has
// to absorb bursts so producers rarely block; the loop drains it immediately, so
// a modest buffer is enough and a full buffer merely makes AddAfter wait (with a
// shutdown escape) rather than dropping anything.
const waitingBuffer = 1000

// NewQueue returns an empty queue that schedules delays on clk.
func NewQueue[T comparable](clk clock.Timing) *Queue[T] {
	q := &Queue[T]{
		dirty:      map[T]struct{}{},
		processing: map[T]struct{}{},
		clk:        clk,
		shutdownCh: make(chan struct{}),
		loopDone:   make(chan struct{}),
		failures:   map[T]int{},
		baseDelay:  defaultBaseDelay,
		maxDelay:   defaultMaxDelay,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add enqueues item for processing. It is a no-op if item is already waiting
// (dedup) or already being processed (it is remembered and re-queued on Done).
func (q *Queue[T]) Add(item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shuttingDown {
		return
	}
	if _, ok := q.dirty[item]; ok {
		return
	}
	q.dirty[item] = struct{}{}
	if _, ok := q.processing[item]; ok {
		return // re-queued by Done so the same key is never processed concurrently
	}
	q.queue = append(q.queue, item)
	q.cond.Signal()
}

// Get blocks until an item is ready or the queue shuts down. The returned item is
// marked as processing; the caller MUST call Done(item) when finished so a re-add
// that arrived during processing is re-queued. shutdown is true only when the
// queue is draining and empty.
func (q *Queue[T]) Get() (item T, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		var zero T
		return zero, true
	}
	item = q.queue[0]
	q.queue = q.queue[1:]
	if len(q.queue) == 0 {
		q.queue = nil // release the backing array once drained
	}
	q.processing[item] = struct{}{}
	delete(q.dirty, item)
	return item, false
}

// Done marks item as finished. If item was re-added while it was being processed,
// it is re-queued now (and only now), preserving the "never concurrent per key"
// guarantee.
func (q *Queue[T]) Done(item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, item)
	if _, ok := q.dirty[item]; ok {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	}
}

// Len is the number of items ready to be fetched (excludes in-flight items).
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// AddAfter enqueues item once d has elapsed on the queue's clock, without
// blocking the caller. A d <= 0 adds immediately. Repeated AddAfter for the same
// item are harmless: each fires an Add, and Add dedups. The timer is created
// synchronously (registered on the clock before AddAfter returns) so a Manual
// clock advanced past d fires it deterministically.
//
// Rather than one goroutine per delayed item, all delayed adds are handed to a
// single waiting loop (started on first use) that holds them in a deadline
// ordered heap and enqueues each when the clock reaches its deadline. The timer
// is created here under q.mu so a concurrent ShutDown either observes it (and the
// loop stops it) or has already set shuttingDown (and this add is dropped). Only
// the loop ever stops a timer it is waiting on, so there is no cross-goroutine
// race on the timer channel.
func (q *Queue[T]) AddAfter(item T, d time.Duration) {
	if d <= 0 {
		q.Add(item)
		return
	}
	q.mu.Lock()
	if q.shuttingDown {
		q.mu.Unlock()
		return
	}
	e := waitFor[T]{item: item, deadline: q.clk.Now().Add(d), timer: q.clk.NewTimer(d)}
	q.ensureLoopLocked()
	q.mu.Unlock()
	select {
	case q.waitingCh <- e:
	case <-q.shutdownCh:
		e.timer.Stop() // the loop is exiting and will not consume this add
	}
}

// ensureLoopLocked starts the single waiting loop the first time a delayed add
// needs it, so a queue that only ever uses Add pays for no extra goroutine.
// Caller holds q.mu, so the started flag and the shuttingDown check in AddAfter
// are consistent with ShutDown's join decision.
func (q *Queue[T]) ensureLoopLocked() {
	if q.loopStarted {
		return
	}
	q.loopStarted = true
	q.waitingCh = make(chan waitFor[T], waitingBuffer)
	go q.waitingLoop()
}

// waitingLoop is the single goroutine that owns delayed adds. It keeps a
// deadline ordered heap, enqueues every item whose deadline the clock has
// reached, and otherwise sleeps on the earliest timer until it fires, a new
// delayed add arrives, or the queue shuts down. It owns every timer in the heap:
// nothing else stops or resets them while the loop waits on one.
func (q *Queue[T]) waitingLoop() {
	defer close(q.loopDone)
	pending := &waitHeap[T]{}
	for {
		now := q.clk.Now()
		for pending.Len() > 0 && !(*pending)[0].deadline.After(now) {
			e := heap.Pop(pending).(waitFor[T])
			q.Add(e.item)
		}

		var next <-chan time.Time
		if pending.Len() > 0 {
			next = (*pending)[0].timer.C()
		}
		select {
		case <-q.shutdownCh:
			q.stopPending(pending)
			return
		case <-next:
			// The earliest timer fired; loop to drain everything now due.
		case e := <-q.waitingCh:
			heap.Push(pending, e)
		}
	}
}

// stopPending stops every timer the loop still holds on shutdown: those in the
// heap plus any already handed off but not yet taken from the channel. It leaves
// nothing armed once the loop returns. A late AddAfter that pushes after this
// drain stops its own timer via the shutdownCh arm of its select.
func (q *Queue[T]) stopPending(pending *waitHeap[T]) {
	for _, e := range *pending {
		e.timer.Stop()
	}
	for {
		select {
		case e := <-q.waitingCh:
			e.timer.Stop()
		default:
			return
		}
	}
}

// waitHeap is a min-heap of delayed adds ordered by deadline, owned solely by the
// waiting loop.
type waitHeap[T comparable] []waitFor[T]

func (h waitHeap[T]) Len() int           { return len(h) }
func (h waitHeap[T]) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h waitHeap[T]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *waitHeap[T]) Push(x any)        { *h = append(*h, x.(waitFor[T])) }
func (h *waitHeap[T]) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

// AddRateLimited enqueues item after its current backoff delay, growing the delay
// on each call (per-item exponential backoff) until Forget resets it. This is how
// a reconcile that errors is retried without a hot loop.
func (q *Queue[T]) AddRateLimited(item T) {
	q.AddAfter(item, q.backoff(item))
}

// backoff returns the next delay for item and records the attempt: baseDelay
// doubling each failure, capped at maxDelay.
func (q *Queue[T]) backoff(item T) time.Duration {
	q.rlMu.Lock()
	defer q.rlMu.Unlock()
	n := q.failures[item]
	q.failures[item] = n + 1
	d := q.baseDelay
	for i := 0; i < n && d < q.maxDelay; i++ {
		d *= 2
	}
	if d <= 0 || d > q.maxDelay {
		d = q.maxDelay
	}
	return d
}

// Forget clears item's backoff so its next AddRateLimited starts from baseDelay
// again. Call it after a successful reconcile.
func (q *Queue[T]) Forget(item T) {
	q.rlMu.Lock()
	defer q.rlMu.Unlock()
	delete(q.failures, item)
}

// NumRequeues is how many times item has been rate-limited since the last Forget.
func (q *Queue[T]) NumRequeues(item T) int {
	q.rlMu.Lock()
	defer q.rlMu.Unlock()
	return q.failures[item]
}

// ShutDown stops the queue: Get returns shutdown once the ready items drain,
// pending AddAfter timers are abandoned, and the call waits for the waiting loop
// to exit (if it was ever started). It is idempotent.
func (q *Queue[T]) ShutDown() {
	q.closeOnce.Do(func() { close(q.shutdownCh) })
	q.mu.Lock()
	q.shuttingDown = true
	q.cond.Broadcast()
	started := q.loopStarted
	q.mu.Unlock()
	if started {
		<-q.loopDone
	}
}
