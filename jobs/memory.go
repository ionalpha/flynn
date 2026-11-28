package jobs

import (
	"context"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

// MemoryQueue is the reference [Queue], held in memory. It honours leases, so it
// runs the same crash-recovery path as a durable backend without surviving a
// restart.
type MemoryQueue struct {
	mu   sync.Mutex
	jobs map[string]*Job
	// active holds the pending and running jobs per queue, so Claim does not scan
	// every job ever created. Terminal jobs stay in jobs for Get.
	active map[string]map[string]*Job
	ready  chan struct{} // capacity 1, for [Notify]

	clk        clock.Clock
	gen        *ids.Generator
	instanceID string
}

// Option configures a MemoryQueue.
type Option func(*MemoryQueue)

// WithClock sets the time source; the default is clock.System.
func WithClock(c clock.Clock) Option {
	return func(q *MemoryQueue) {
		if c != nil {
			q.clk = c
		}
	}
}

// WithIDGenerator sets the job ID source. A seeded generator makes IDs
// reproducible.
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

// NewMemory returns an empty queue.
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

// Ready implements [Waker]. Recover also signals it.
func (q *MemoryQueue) Ready() <-chan struct{} { return q.ready }

// index requires mu.
func (q *MemoryQueue) index(j *Job) {
	byID, ok := q.active[j.Queue]
	if !ok {
		byID = make(map[string]*Job)
		q.active[j.Queue] = byID
	}
	byID[j.ID] = j
}

// evict removes a terminal job from active. It requires mu.
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

	// The map holds its own copy, because Claim mutates it in place and a returned
	// alias would race.
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

	ready := make([]*Job, 0)
	//ordered:ok each timeout touches only its job, and ready is sorted below by a total order
	for _, j := range q.active[queue] {
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
	// The total order every backend uses, so all hand out the same jobs.
	sort.SliceStable(ready, func(a, b int) bool {
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

// claimReady requires mu.
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

// Recover implements Queue.
func (q *MemoryQueue) Recover(context.Context) (int, error) {
	now := q.clk.Now().UnixNano()
	q.mu.Lock()
	n := 0
	//ordered:ok each job is recovered on its own
	for _, byID := range q.active {
		//ordered:ok each job is recovered on its own
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
