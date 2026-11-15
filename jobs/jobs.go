// Package jobs defines the durable, at-least-once work queue, so a crash between
// deciding to do work and finishing it loses nothing. Workers claim jobs under a
// lease, and a crashed worker's jobs return to the queue when the lease expires.
//
// [MemoryQueue] is the reference backend; every backend runs the jobstest suite and
// uses the transition functions here, so all make the same decisions.
package jobs

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// State is a job's lifecycle position.
type State string

const (
	// StatePending is claimable once RunAt has passed.
	StatePending State = "pending"
	// StateRunning is held under a lease; an expired lease makes it claimable again.
	StateRunning State = "running"
	// StateDone is terminal.
	StateDone State = "done"
	// StateDead is terminal and kept for inspection.
	StateDead State = "dead"
)

// DefaultQueue is used when a Queue field is empty.
const DefaultQueue = "default"

// Job is one unit of durable work. The queue never interprets Kind or Payload.
type Job struct {
	ID      string
	Queue   string
	Kind    string // selects the handler
	Payload []byte
	Scope   state.Scope

	State       State
	Attempt     int // attempts started; 0 until first claim
	MaxAttempts int
	LastError   string // fault class and message of the latest failure

	RunAt        int64 // unix nanos; earliest claim time
	LeaseExpires int64 // unix nanos; 0 unless running

	OriginInstanceID string // instance that enqueued the job
	CreatedAt        int64
	UpdatedAt        int64
}

// EnqueueParams describes a job to enqueue. Only Kind is required.
type EnqueueParams struct {
	Queue       string
	Kind        string
	Payload     []byte
	Scope       state.Scope
	MaxAttempts int   // <= 0 uses DefaultMaxAttempts
	RunAt       int64 // unix nanos; 0 means as soon as possible
}

// DefaultMaxAttempts is the total number of attempts, first run included, when
// EnqueueParams.MaxAttempts is unset.
const DefaultMaxAttempts = 25

// ClaimParams describes a worker's request to lease ready jobs.
type ClaimParams struct {
	Queue    string // empty means DefaultQueue
	Limit    int    // <= 0 means 1
	LeaseFor int64  // nanos
}

// Queue is the durable work-queue port. Implementations must be safe for
// concurrent use and must claim each ready job to at most one worker at a time.
type Queue interface {
	// Enqueue returns the job with its ID and defaults applied.
	Enqueue(ctx context.Context, p EnqueueParams) (Job, error)
	// Claim leases up to Limit jobs that are [Claimable], in RunAt, creation and
	// ID order, incrementing each Attempt.
	Claim(ctx context.Context, p ClaimParams) ([]Job, error)
	// Complete returns [ErrNotRunning] for a job that is not running.
	Complete(ctx context.Context, id string) error
	// Fail records a failed attempt: back to pending at retryAt while attempts
	// remain, else dead. A negative retryAt makes the job dead at once.
	Fail(ctx context.Context, id, errMsg string, retryAt int64) error
	Get(ctx context.Context, id string) (Job, error)
	// Recover makes every running job claimable now and returns the count. Startup
	// calls it under the single-instance lock, where no live worker holds a lease,
	// so an interrupted run resumes without waiting out its lease. Attempt counts
	// are unchanged.
	Recover(ctx context.Context) (int, error)
	Close() error
}

// Waker is an optional [Queue] capability that wakes a worker on an in-process
// write instead of its next poll. The signal is advisory and coalesced, so a
// worker still polls: polling is the only way to see a RunAt arriving or a write
// from another process.
type Waker interface {
	// Ready is signalled after an enqueue or a retry. Every call returns the same
	// channel.
	Ready() <-chan struct{}
}

// Notify sends a [Waker] signal without blocking; if one is already pending the
// two merge. Every backend uses it so none can block a writer on a slow worker.
func Notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Sentinel errors.
var (
	ErrNotFound = fault.New(fault.Terminal, "job_not_found", "job not found")
	// ErrNotRunning covers a job that is done, dead or never claimed.
	ErrNotRunning = fault.New(fault.Terminal, "job_not_running", "job is not running")
	// ErrInvalidJob is an empty Kind.
	ErrInvalidJob = fault.New(fault.Terminal, "job_invalid", "invalid job")
)

// BuildJob returns the job Enqueue stores, with defaults applied. A backend
// supplies storage and atomic claim selection; the functions from here down are
// the transitions, shared so every backend decides the same way. The caller
// checks p.Kind first.
func BuildJob(p EnqueueParams, now int64, id, instanceID string) Job {
	queue := p.Queue
	if queue == "" {
		queue = DefaultQueue
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	runAt := p.RunAt
	if runAt == 0 {
		runAt = now
	}
	return Job{
		ID:               id,
		Queue:            queue,
		Kind:             p.Kind,
		Payload:          p.Payload,
		Scope:            p.Scope,
		State:            StatePending,
		MaxAttempts:      maxAttempts,
		RunAt:            runAt,
		OriginInstanceID: instanceID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// ClaimDefaults returns p's queue and limit with defaults applied.
func ClaimDefaults(p ClaimParams) (queue string, limit int) {
	queue = p.Queue
	if queue == "" {
		queue = DefaultQueue
	}
	limit = p.Limit
	if limit <= 0 {
		limit = 1
	}
	return queue, limit
}

// Claimable reports whether j is pending with RunAt passed, or running with an
// expired lease and attempts left. Each claim is an attempt, so an expired job
// with none left is [ExpiredExhausted] and never exceeds MaxAttempts.
func Claimable(j Job, now int64) bool {
	switch j.State {
	case StatePending:
		return j.RunAt <= now
	case StateRunning:
		return j.LeaseExpires <= now && j.Attempt < j.MaxAttempts
	default:
		return false
	}
}

// ExpiredExhausted reports whether a running job's lease expired on its last
// attempt. Claim reaps such a job to dead.
func ExpiredExhausted(j Job, now int64) bool {
	return j.State == StateRunning && j.LeaseExpires <= now && j.Attempt >= j.MaxAttempts
}

// MarkTimedOut makes an [ExpiredExhausted] job dead.
func MarkTimedOut(j *Job, now int64) {
	j.State = StateDead
	j.LastError = "lease expired without completion"
	j.LeaseExpires = 0
	j.UpdatedAt = now
}

// MarkRecovered expires j's lease, leaving Attempt alone, so a job with no
// attempts left is reaped by the next Claim instead of revived.
func MarkRecovered(j *Job, now int64) {
	j.LeaseExpires = 0
	j.UpdatedAt = now
}

// MarkClaimed starts an attempt under a lease ending leaseFor nanos after now.
func MarkClaimed(j *Job, now, leaseFor int64) {
	j.State = StateRunning
	j.Attempt++
	j.LeaseExpires = now + leaseFor
	j.UpdatedAt = now
}

// MarkDone makes j done and clears its lease.
func MarkDone(j *Job, now int64) {
	j.State = StateDone
	j.LeaseExpires = 0
	j.UpdatedAt = now
}

// MarkFailed applies [Queue.Fail]'s rule. A negative retryAt marks a cause that
// cannot succeed, so no attempts are spent on it.
func MarkFailed(j *Job, errMsg string, retryAt, now int64) {
	j.LastError = errMsg
	j.LeaseExpires = 0
	j.UpdatedAt = now
	if retryAt < 0 || j.Attempt >= j.MaxAttempts {
		j.State = StateDead
		return
	}
	j.State = StatePending
	j.RunAt = retryAt
}

// Backoff returns the delay before retry number attempt (1-based; lower counts as
// 1), doubling from base and capped at ceiling.
func Backoff(attempt int, base, ceiling int64) int64 {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		// d < base catches int64 overflow.
		if d >= ceiling || d < base {
			return ceiling
		}
	}
	if d > ceiling {
		return ceiling
	}
	return d
}
