// Package reconcile is the agent's desired-state execution engine: a small,
// in-process control loop in the Kubernetes mould (a level-triggered reconciler
// driven by a deduplicating work queue) but with no cluster, no apiserver, and no
// etcd. It runs in one binary over the resource store, the event log, and the job
// queue. The control-theory mechanics are borrowed (they are domain-agnostic and
// proven); the engine on top, reconciling agent goals toward an LLM-judged stop
// condition, is the agent's own.
//
// This file is the load-bearing primitive: the work queue. It mirrors the
// semantics of client-go's controller workqueue (a key is never processed
// concurrently; adds collapse while a key waits; delayed and rate-limited re-adds
// avoid hot loops) without importing it, and it schedules delays through a
// clock.Timing source so every delay is deterministic under a Manual clock.
package reconcile

import (
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

	clk        clock.Timing
	shutdownCh chan struct{}
	closeOnce  sync.Once
	wg         sync.WaitGroup // pending AddAfter timers

	rlMu      sync.Mutex
	failures  map[T]int
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewQueue returns an empty queue that schedules delays on clk.
func NewQueue[T comparable](clk clock.Timing) *Queue[T] {
	q := &Queue[T]{
		dirty:      map[T]struct{}{},
		processing: map[T]struct{}{},
		clk:        clk,
		shutdownCh: make(chan struct{}),
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
func (q *Queue[T]) AddAfter(item T, d time.Duration) {
	if d <= 0 {
		q.Add(item)
		return
	}
	select {
	case <-q.shutdownCh:
		return
	default:
	}
	t := q.clk.NewTimer(d)
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		defer t.Stop()
		select {
		case <-t.C():
			q.Add(item)
		case <-q.shutdownCh:
		}
	}()
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
// pending AddAfter timers are abandoned, and the call waits for their goroutines
// to exit. It is idempotent.
func (q *Queue[T]) ShutDown() {
	q.closeOnce.Do(func() { close(q.shutdownCh) })
	q.mu.Lock()
	q.shuttingDown = true
	q.cond.Broadcast()
	q.mu.Unlock()
	q.wg.Wait()
}
