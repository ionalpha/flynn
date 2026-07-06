package jobs

import (
	"context"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

// MemoryQueue is the in-process reference Queue: a map guarded by a mutex, no
// persistence. It is the fast default for tests and ephemeral runs, and the
// reference semantics the durable SQLite backend must match (both run
// jobstest.RunSuite). Lease expiry is honoured, so it exercises the same
// crash-recovery path as a durable backend, just without surviving a restart.
type MemoryQueue struct {
	mu   sync.Mutex
	jobs map[string]*Job
	// active indexes the non-terminal jobs per queue (pending or running), so
	// Claim scans only live work. Terminal jobs (done, dead) leave this index the
	// moment they settle but stay in jobs for Get: retention for inspection is
	// unchanged, they just no longer cost the claim path anything. Without this,
	// Claim is O(every job ever created) on a long-lived server.
	active map[string]map[string]*Job
	// ready carries the Waker signal: one buffered slot, written by Enqueue and
	// retry-Fail via Notify, drained by a worker between claims.
	ready chan struct{}

	clk        clock.Clock
	gen        *ids.Generator
	instanceID string
}

// Option configures a MemoryQueue.
type Option func(*MemoryQueue)

// WithClock sets the time source (default: clock.System). Tests and deterministic
// replay pass a clock.Manual.
func WithClock(c clock.Clock) Option {
	return func(q *MemoryQueue) {
		if c != nil {
			q.clk = c
		}
	}
}

// WithIDGenerator sets the job-ID source (default: a generator on the queue's
// clock). A seeded generator makes enqueued IDs reproducible.
func WithIDGenerator(g *ids.Generator) Option {
	return func(q *MemoryQueue) {
		if g != nil {
			q.gen = g
		}
	}
}

// WithInstanceID sets the instance stamped onto enqueued jobs (default "local").
func WithInstanceID(id string) Option {
	return func(q *MemoryQueue) {
		if id != "" {
			q.instanceID = id
		}
	}
}

// NewMemory constructs an in-process Queue ready to use with zero configuration.
func NewMemory(opts ...Option) *MemoryQueue {
	q := &MemoryQueue{
		jobs:       make(map[string]*Job),
		active:     make(map[string]map[string]*Job),
		ready:      make(chan struct{}, 1),
		clk:        clock.System{},
		instanceID: "local",
	}
	for _, o := range opts {
		o(q)
	}
	if q.gen == nil {
		q.gen = ids.NewGenerator(ids.WithClock(q.clk))
	}
	return q
}

var (
	_ Queue = (*MemoryQueue)(nil)
	_ Waker = (*MemoryQueue)(nil)
)

// Ready implements Waker: the channel signalled after Enqueue and after a Fail
// that returns a job to pending.
func (q *MemoryQueue) Ready() <-chan struct{} { return q.ready }

// index adds a live job to its queue's claim index. Caller holds mu.
func (q *MemoryQueue) index(j *Job) {
	byID, ok := q.active[j.Queue]
	if !ok {
		byID = make(map[string]*Job)
		q.active[j.Queue] = byID
	}
	byID[j.ID] = j
}

// evict removes a job that reached a terminal state from the claim index. The
// job remains in q.jobs for Get. Caller holds mu.
func (q *MemoryQueue) evict(j *Job) {
	byID, ok := q.active[j.Queue]
	if !ok {
		return
	}
	delete(byID, j.ID)
	if len(byID) == 0 {
		delete(q.active, j.Queue)
	}
}

// Enqueue implements Queue.
func (q *MemoryQueue) Enqueue(_ context.Context, p EnqueueParams) (Job, error) {
	if p.Kind == "" {
		return Job{}, ErrInvalidJob
	}
	now := q.clk.Now().UnixNano()
	j := BuildJob(p, now, q.gen.New(), q.instanceID)

	// The map keeps its own copy: a later Claim mutates the stored job in place
	// through this pointer, so the returned value must not alias it, or copying it
	// out here would race with that mutation.
	stored := j
	q.mu.Lock()
	q.jobs[stored.ID] = &stored
	q.index(&stored)
	q.mu.Unlock()
	Notify(q.ready)
	return j, nil
}

// Claim implements Queue.
func (q *MemoryQueue) Claim(_ context.Context, p ClaimParams) ([]Job, error) {
	queue, limit := ClaimDefaults(p)
	now := q.clk.Now().UnixNano()

	q.mu.Lock()
	defer q.mu.Unlock()

	// Only the queue's live jobs are scanned; terminal jobs left this index when
	// they settled, so a long-lived server's done/dead backlog costs nothing here.
	ready := make([]*Job, 0)
	for _, j := range q.active[queue] {
		// Reap jobs that timed out on their last attempt before considering work
		// to hand out, so an exhausted zombie becomes dead rather than lingering.
		if ExpiredExhausted(*j, now) {
			MarkTimedOut(j, now)
			q.evict(j) // deleting while ranging a map is safe in Go
			continue
		}
		if Claimable(*j, now) {
			ready = append(ready, j)
		}
	}
	if len(ready) < 2 {
		return claimReady(ready, now, p.LeaseFor), nil
	}
	// Stable ordering: earliest RunAt first, then creation order, then ID. This is
	// the same total order the SQLite backend's ORDER BY produces, so both
	// backends hand the same jobs to the same workers.
	sort.Slice(ready, func(a, b int) bool {
		ja, jb := ready[a], ready[b]
		if ja.RunAt != jb.RunAt {
			return ja.RunAt < jb.RunAt
		}
		if ja.CreatedAt != jb.CreatedAt {
			return ja.CreatedAt < jb.CreatedAt
		}
		return ja.ID < jb.ID
	})
	if len(ready) > limit {
		ready = ready[:limit]
	}
	return claimReady(ready, now, p.LeaseFor), nil
}

// claimReady leases the selected jobs and copies them out. Caller holds mu.
func claimReady(ready []*Job, now, leaseFor int64) []Job {
	out := make([]Job, 0, len(ready))
	for _, j := range ready {
		MarkClaimed(j, now, leaseFor)
		out = append(out, *j)
	}
	return out
}

// Complete implements Queue.
func (q *MemoryQueue) Complete(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.State != StateRunning {
		return ErrNotRunning
	}
	MarkDone(j, q.clk.Now().UnixNano())
	q.evict(j)
	return nil
}

// Fail implements Queue.
func (q *MemoryQueue) Fail(_ context.Context, id, errMsg string, retryAt int64) error {
	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return ErrNotFound
	}
	if j.State != StateRunning {
		q.mu.Unlock()
		return ErrNotRunning
	}
	MarkFailed(j, errMsg, retryAt, q.clk.Now().UnixNano())
	if j.State == StateDead {
		q.evict(j)
	}
	pending := j.State == StatePending
	q.mu.Unlock()
	// A retryable failure re-pends the job; wake a worker so a due retry is
	// picked up on the signal rather than the next idle poll.
	if pending {
		Notify(q.ready)
	}
	return nil
}

// Get implements Queue.
func (q *MemoryQueue) Get(_ context.Context, id string) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return *j, nil
}

// Recover implements Queue: it expires the lease of every job left running, so the next
// Claim reclaims (or, if attempts are spent, reaps) it at once. Only live jobs are
// scanned; terminal jobs already left the active index.
func (q *MemoryQueue) Recover(context.Context) (int, error) {
	now := q.clk.Now().UnixNano()
	q.mu.Lock()
	n := 0
	for _, byID := range q.active {
		for _, j := range byID {
			if j.State == StateRunning {
				MarkRecovered(j, now)
				n++
			}
		}
	}
	q.mu.Unlock()
	if n > 0 {
		Notify(q.ready)
	}
	return n, nil
}

// Close implements Queue.
func (q *MemoryQueue) Close() error { return nil }
