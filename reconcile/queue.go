// Package reconcile is an in-process, level-triggered control loop in the Kubernetes
// style: reconcilers driven by a deduplicating work queue over the resource store, with
// no cluster or apiserver.
//
// [Queue] follows client-go's workqueue semantics (a key is never processed
// concurrently, adds collapse while a key waits, delayed re-adds back off) and schedules
// every delay on a clock.Timing, so delays are deterministic under a Manual clock.
package reconcile

import (
	"container/heap"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Backoff bounds for AddRateLimited, the client-go controller defaults.
const (
	defaultBaseDelay = 5 * time.Millisecond
	defaultMaxDelay  = 1000 * time.Second
)

// Queue is a deduplicating work queue keyed by T. A key added many times before it is
// fetched is processed once; a re-add during processing re-queues the key once, on
// [Queue.Done]. T is an identity with no payload, so the reconciler always re-reads
// current state.
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

// waitFor is one delayed add from AddAfter to the waiting loop.
type waitFor[T comparable] struct {
	item     T
	deadline time.Time
	timer    clock.Timer
}

// waitingBuffer absorbs bursts of AddAfter. A full buffer makes AddAfter wait, with a
// shutdown escape, and never drops an add.
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

// Add enqueues item. An item already waiting is not added twice; one being processed is
// re-queued on Done.
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

// Get blocks until an item is ready or the queue shuts down. The caller must call
// [Queue.Done] with the item, or a re-add during processing is lost. shutdown is true
// only once the queue is shut down and empty.
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

// Done marks item finished and re-queues it if it was re-added meanwhile, so a key is
// never processed concurrently.
func (q *Queue[T]) Done(item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, item)
	if _, ok := q.dirty[item]; ok {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	}
}

// Len counts items ready to fetch, excluding those in flight.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// AddAfter enqueues item once d has elapsed on the queue's clock; d <= 0 adds now. The
// timer is registered before AddAfter returns, so a Manual clock advanced past d fires
// it. One waiting loop holds every delayed add in a deadline heap. The timer is created
// under q.mu, so a concurrent ShutDown either sees it and the loop stops it, or has set
// shuttingDown and the add is dropped. Only the loop stops a timer it waits on.
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

// ensureLoopLocked starts the waiting loop on the first delayed add, so a queue using
// only Add runs no extra goroutine. The caller holds q.mu, which keeps loopStarted
// consistent with ShutDown's decision to wait for the loop.
func (q *Queue[T]) ensureLoopLocked() {
	if q.loopStarted {
		return
	}
	q.loopStarted = true
	q.waitingCh = make(chan waitFor[T], waitingBuffer)
	go q.waitingLoop()
}

// waitingLoop owns every delayed add and its timer, so nothing else stops or resets a
// timer while the loop waits on it.
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

// stopPending stops every timer in the heap and in the channel on shutdown. A later
// AddAfter stops its own timer through the shutdownCh arm of its select.
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

// waitHeap is a min-heap of delayed adds by deadline, owned by the waiting loop.
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

// AddRateLimited enqueues item after its backoff delay, which doubles on each call until
// [Queue.Forget] resets it.
func (q *Queue[T]) AddRateLimited(item T) {
	q.AddAfter(item, q.backoff(item))
}

// backoff records an attempt and returns the next delay.
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

// Forget resets item's backoff. Call it after a successful reconcile.
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

// ShutDown makes Get return shutdown once ready items drain, abandons pending AddAfter
// timers, and waits for the waiting loop to exit. It is idempotent.
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
