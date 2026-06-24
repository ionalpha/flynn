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

var _ Queue = (*MemoryQueue)(nil)

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
	q.mu.Unlock()
	return j, nil
}

// Claim implements Queue.
func (q *MemoryQueue) Claim(_ context.Context, p ClaimParams) ([]Job, error) {
	queue, limit := ClaimDefaults(p)
	now := q.clk.Now().UnixNano()

	q.mu.Lock()
	defer q.mu.Unlock()

	ready := make([]*Job, 0)
	for _, j := range q.jobs {
		if j.Queue != queue {
			continue
		}
		// Reap jobs that timed out on their last attempt before considering work
		// to hand out, so an exhausted zombie becomes dead rather than lingering.
		if ExpiredExhausted(*j, now) {
			MarkTimedOut(j, now)
			continue
		}
		if Claimable(*j, now) {
			ready = append(ready, j)
		}
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

	out := make([]Job, 0, len(ready))
	for _, j := range ready {
		MarkClaimed(j, now, p.LeaseFor)
		out = append(out, *j)
	}
	return out, nil
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
	return nil
}

// Fail implements Queue.
func (q *MemoryQueue) Fail(_ context.Context, id, errMsg string, retryAt int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.State != StateRunning {
		return ErrNotRunning
	}
	MarkFailed(j, errMsg, retryAt, q.clk.Now().UnixNano())
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

// Close implements Queue.
func (q *MemoryQueue) Close() error { return nil }
