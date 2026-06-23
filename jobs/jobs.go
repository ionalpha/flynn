// Package jobs is the agent's durable work queue: a port for work that must
// survive a restart and be retried until it succeeds or is exhausted. Scheduled
// automations, delayed and retried side effects, and fan-out work are enqueued
// here rather than run inline, so a crash between "decide to do X" and "X done"
// loses nothing.
//
// Like state, spine, and observe, the queue is a PORT with a zero-dependency
// default and heavy backends as opt-in adapters. The default is SQLite-backed
// (durable, single file, no setup); a fast in-memory queue is the reference for
// tests and ephemeral use. River (Postgres) and NATS/JetStream (cross-process,
// fleet, k8s) attach later as adapters held to the same jobstest conformance
// suite. Workers claim jobs under a lease, so a crashed worker's in-flight jobs
// return to the queue when the lease expires rather than being lost.
//
// The queue is distinct from its neighbours: the bus is at-most-once ambient
// signalling, the spine is the durable ordered truth log. The queue is durable,
// at-least-once, retried work that the bus and spine are not.
package jobs

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// State is a job's lifecycle position.
type State string

const (
	// StatePending is ready to run at or after RunAt, awaiting a worker.
	StatePending State = "pending"
	// StateRunning is claimed by a worker under an unexpired lease.
	StateRunning State = "running"
	// StateDone is completed successfully; terminal.
	StateDone State = "done"
	// StateDead is failed and out of attempts; terminal, retained for inspection.
	StateDead State = "dead"
)

// DefaultQueue is the lane a job lands in when EnqueueParams.Queue is empty.
const DefaultQueue = "default"

// Job is one unit of durable work. Payload is opaque bytes so the queue is
// agnostic to encoding; the typed-handler layer marshals it.
type Job struct {
	ID      string
	Queue   string
	Kind    string // selects the handler; the queue itself never interprets it
	Payload []byte
	Scope   state.Scope

	State       State
	Attempt     int    // attempts started so far (0 until first claim)
	MaxAttempts int    // attempts allowed before the job goes dead
	LastError   string // fault class + message from the most recent failure

	RunAt        int64 // unix nanos: earliest time the job may be claimed
	LeaseExpires int64 // unix nanos: when a running job's claim lapses (0 if not running)

	OriginInstanceID string // instance that enqueued the job, for fleet attribution
	CreatedAt        int64
	UpdatedAt        int64
}

// EnqueueParams describes a job to enqueue. Only Kind is required; the queue
// fills in safe defaults (DefaultQueue, MaxAttempts, RunAt = now).
type EnqueueParams struct {
	Queue       string
	Kind        string
	Payload     []byte
	Scope       state.Scope
	MaxAttempts int   // <= 0 uses DefaultMaxAttempts
	RunAt       int64 // unix nanos; 0 means as soon as possible
}

// DefaultMaxAttempts is the retry ceiling applied when EnqueueParams.MaxAttempts
// is unset. The job runs once and is retried up to this many times in total.
const DefaultMaxAttempts = 25

// ClaimParams describes a worker's request to lease ready jobs.
type ClaimParams struct {
	Queue    string // empty means DefaultQueue
	Limit    int    // max jobs to claim; <= 0 means 1
	LeaseFor int64  // lease duration in nanos; the claim is held this long
}

// Queue is the durable work-queue port. Implementations must be safe for
// concurrent use and must claim each ready job to at most one worker at a time.
type Queue interface {
	// Enqueue persists a new job and returns it with its assigned ID and defaults
	// applied.
	Enqueue(ctx context.Context, p EnqueueParams) (Job, error)
	// Claim leases up to Limit ready jobs from a queue: jobs that are pending with
	// RunAt in the past, or running jobs whose lease has expired (a crashed
	// worker's work). Claimed jobs move to StateRunning with their Attempt
	// incremented and a fresh lease, and are returned to the caller.
	Claim(ctx context.Context, p ClaimParams) ([]Job, error)
	// Complete marks a claimed job done. It is an error to complete a job that is
	// not running.
	Complete(ctx context.Context, id string) error
	// Fail records a failed attempt. If attempts remain the job returns to pending
	// with RunAt = retryAt (the caller computes backoff); otherwise it goes dead.
	Fail(ctx context.Context, id, errMsg string, retryAt int64) error
	// Get returns a job by ID.
	Get(ctx context.Context, id string) (Job, error)
	// Close releases the queue's resources.
	Close() error
}

// Sentinel errors, fault-classified so callers branch on the class rather than
// on string matching.
var (
	// ErrNotFound is returned by Get/Complete/Fail for an unknown job ID.
	ErrNotFound = fault.New(fault.Terminal, "job_not_found", "job not found")
	// ErrNotRunning is returned by Complete/Fail when the job is not currently
	// leased (already done, dead, or never claimed).
	ErrNotRunning = fault.New(fault.Terminal, "job_not_running", "job is not running")
	// ErrInvalidJob is returned by Enqueue for params that cannot describe a job
	// (an empty Kind).
	ErrInvalidJob = fault.New(fault.Terminal, "job_invalid", "invalid job")
)

// --- shared lifecycle logic (one source of truth for every backend) ---------
//
// These functions hold the queue's semantics so the in-memory, SQLite, and any
// future adapter make byte-for-byte identical enqueue/claim/retry decisions. A
// backend only supplies storage and atomic claim selection; the transitions
// themselves live here, which is why jobstest can hold every backend to the same
// contract.

// BuildJob constructs the job an Enqueue stores, applying defaults (DefaultQueue,
// DefaultMaxAttempts, RunAt = now). The caller validates that p.Kind is non-empty
// and supplies the assigned id, now (unix nanos), and origin instance.
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

// ClaimDefaults resolves a ClaimParams to a concrete queue and limit.
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

// Claimable reports whether a job may be claimed at time now: a pending job whose
// RunAt has arrived, or a running job whose lease has expired and still has
// attempts left (crash recovery). Each claim is an attempt, so a job whose lease
// expires after its final attempt is not reclaimed (it is timed out, see
// ExpiredExhausted) rather than retried past MaxAttempts.
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

// ExpiredExhausted reports whether a running job has timed out on its final
// attempt: its lease has expired and no attempts remain. Such a job is reaped to
// dead by Claim rather than left as a zombie or retried forever.
func ExpiredExhausted(j Job, now int64) bool {
	return j.State == StateRunning && j.LeaseExpires <= now && j.Attempt >= j.MaxAttempts
}

// MarkTimedOut transitions a job that exhausted its attempts by lease expiry to
// dead, recording why. Used by Claim's reap step.
func MarkTimedOut(j *Job, now int64) {
	j.State = StateDead
	j.LastError = "lease expired without completion"
	j.LeaseExpires = 0
	j.UpdatedAt = now
}

// MarkClaimed transitions a job into a fresh lease: running, one more attempt,
// and a lease ending leaseFor nanos from now.
func MarkClaimed(j *Job, now, leaseFor int64) {
	j.State = StateRunning
	j.Attempt++
	j.LeaseExpires = now + leaseFor
	j.UpdatedAt = now
}

// MarkDone transitions a job to done and clears its lease.
func MarkDone(j *Job, now int64) {
	j.State = StateDone
	j.LeaseExpires = 0
	j.UpdatedAt = now
}

// MarkFailed transitions a running job after a failed attempt: back to pending
// (RunAt = retryAt) while attempts remain, or dead once they are exhausted.
func MarkFailed(j *Job, errMsg string, retryAt, now int64) {
	j.LastError = errMsg
	j.LeaseExpires = 0
	j.UpdatedAt = now
	if j.Attempt >= j.MaxAttempts {
		j.State = StateDead
		return
	}
	j.State = StatePending
	j.RunAt = retryAt
}

// Backoff returns the delay before the attempt-th retry (1-based), doubling from
// base and capped at ceiling. It is the default exponential policy a worker
// applies when computing Fail's retryAt; callers may substitute their own.
// attempt <= 0 is treated as 1.
func Backoff(attempt int, base, ceiling int64) int64 {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		// Doubling can overflow int64 into a negative value; treat any overflow
		// or cap breach as the ceiling.
		if d >= ceiling || d < base {
			return ceiling
		}
	}
	if d > ceiling {
		return ceiling
	}
	return d
}
