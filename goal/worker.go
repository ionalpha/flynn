package goal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// StepQueue is the job queue goal steps are dispatched on, so a step worker claims
// only goal steps and not unrelated jobs.
const StepQueue = "goal-steps"

// StepSubject is the bus subject a worker publishes on when a step completes, so
// the reconciler is woken to re-evaluate promptly (the resync is the fallback).
const StepSubject = "goal.step.done"

// DefaultLease is how long a claimed step is leased before, if the worker has not
// completed or renewed it, the queue treats the worker as crashed and re-leases
// the step to another worker. That re-lease is the crash-recovery path.
const DefaultLease = 5 * time.Minute

// DefaultRetryBase and DefaultRetryCeiling bound the exponential backoff a worker
// applies when a step fails: the first retry waits DefaultRetryBase, each later
// retry doubles, capped at DefaultRetryCeiling. Backing off matters because a step
// calls the model and external tools; retrying a persistently failing step with no
// delay would burn the attempt budget in microseconds and hammer those services.
const (
	DefaultRetryBase    = 2 * time.Second
	DefaultRetryCeiling = 5 * time.Minute
)

// StepExecutor performs one step of work toward a goal: it is where the model is
// called, tools are run, and sub-goals are planned. It is handed the goal resource
// (whose status carries the last Checkpoint) and returns a new checkpoint to
// persist. It MUST be safe to re-run after a crash: a re-leased step calls Execute
// again with the persisted checkpoint, so the executor resumes from it rather than
// repeating finished work. The real executor is wired in with the conversation
// loop; the foundation here is provider-agnostic.
type StepExecutor interface {
	Execute(ctx context.Context, goal resource.Resource) (checkpoint json.RawMessage, err error)
}

// ErrWaiting is returned by a StepExecutor whose step made no progress because it
// is waiting on external state, such as a fan-out whose children are still
// running. The worker completes the job without persisting a checkpoint and stamps
// Status.WaitingSince, and the reconciler then parks the goal: no step is counted
// against the budget and no re-check job is dispatched until a child settles (the
// wake) or the recheck fallback fires. Without this, a wait is a full durable step
// per poll cycle, and a long enough wait exhausts the step budget and false-stalls
// the goal.
var ErrWaiting = errors.New("goal: step waiting on external state")

// Worker claims dispatched goal steps and runs them through a StepExecutor. It is
// the execution half of the goal reconciler's dispatch-and-observe loop: the
// reconciler decides a step is needed and enqueues it; the worker performs it,
// persists progress, and signals completion so the reconciler observes the result.
type Worker struct {
	store     resource.Store
	jobs      jobs.Queue
	exec      StepExecutor
	clk       clock.Timing
	bus       bus.Bus // optional; nil disables completion signals
	lease     time.Duration
	retryBase time.Duration
	retryCeil time.Duration
}

// WorkerOption configures a Worker.
type WorkerOption func(*Worker)

// WithBus sets the bus a worker publishes step-completion signals on.
func WithBus(b bus.Bus) WorkerOption { return func(w *Worker) { w.bus = b } }

// WithLease overrides the step lease duration.
func WithLease(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d > 0 {
			w.lease = d
		}
	}
}

// WithBackoff overrides the failed-step retry backoff (base delay and ceiling).
func WithBackoff(base, ceiling time.Duration) WorkerOption {
	return func(w *Worker) {
		if base > 0 {
			w.retryBase = base
		}
		if ceiling > 0 {
			w.retryCeil = ceiling
		}
	}
}

// NewWorker builds a goal-step worker over the store, queue and executor. The
// clock is used to schedule retry backoff and must be the same clock the queue
// uses, so a failed step's RunAt is comparable to the queue's claim time.
func NewWorker(store resource.Store, q jobs.Queue, clk clock.Timing, exec StepExecutor, opts ...WorkerOption) *Worker {
	w := &Worker{
		store:     store,
		jobs:      q,
		exec:      exec,
		clk:       clk,
		lease:     DefaultLease,
		retryBase: DefaultRetryBase,
		retryCeil: DefaultRetryCeiling,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// ProcessOnce claims at most one ready step and runs it, reporting whether a step
// was processed. It is the unit of work a Run loop repeats and the entry point tests
// drive deterministically.
func (w *Worker) ProcessOnce(ctx context.Context) (bool, error) {
	claimed, err := w.jobs.Claim(ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(w.lease)})
	if err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	return true, w.runStep(ctx, claimed[0])
}

// Run processes steps until ctx is cancelled. When the queue implements
// jobs.Waker, an idle worker wakes on the enqueue signal, so dispatch-to-claim
// latency is signal delivery, not the poll interval. The poll remains the
// always-correct floor: it is the only wake-up for scheduled RunAt arrivals,
// expired leases, and enqueues from processes the queue cannot observe.
func (w *Worker) Run(ctx context.Context, poll time.Duration) {
	var ready <-chan struct{} // nil (blocks forever in select) unless the queue signals
	if wk, ok := w.jobs.(jobs.Waker); ok {
		ready = wk.Ready()
	}
	// One timer for the whole loop, re-armed each idle wait, instead of a fresh
	// timer (and channel) per iteration from clk.After. Start it stopped; each idle
	// branch Resets it, and the ctx/ready branches Stop-and-drain so the next Reset
	// starts clean.
	timer := w.clk.NewTimer(poll)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C()
	}
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := w.ProcessOnce(ctx)
		if err != nil || !processed {
			timer.Reset(poll)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C()
				}
				return
			case <-ready:
				if !timer.Stop() {
					<-timer.C()
				}
			case <-timer.C():
			}
		}
	}
}

// runStep executes one claimed step: load the goal, run the executor, persist the
// returned checkpoint (best effort), then complete the job and signal. A goal that
// has vanished or is terminating completes the job without work, so a deleting
// goal is not kept alive by a pending step.
func (w *Worker) runStep(ctx context.Context, job jobs.Job) error {
	r, err := w.store.GetByID(ctx, string(job.Payload))
	if errors.Is(err, resource.ErrNotFound) {
		return w.jobs.Complete(ctx, job.ID) // goal gone; nothing to do
	}
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if r.DeletionTimestamp != nil {
		return w.jobs.Complete(ctx, job.ID) // terminating; stop working on it
	}

	checkpoint, err := w.exec.Execute(ctx, r)
	if errors.Is(err, ErrWaiting) {
		w.markWaiting(ctx, r)
		if err := w.jobs.Complete(ctx, job.ID); err != nil {
			return err
		}
		w.signal(ctx, r)
		return nil
	}
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if len(checkpoint) > 0 {
		w.persistCheckpoint(ctx, r, checkpoint) // best effort; never blocks completion
	}
	if err := w.jobs.Complete(ctx, job.ID); err != nil {
		return err // crashed before completing: the lease lapses and the step re-runs
	}
	w.signal(ctx, r)
	return nil
}

// persistCheckpoint records the step's progress on the goal's status so the next
// step resumes from it. A version conflict does NOT mean the checkpoint may be
// dropped: the reconciler dispatches the job before it persists the in-flight
// status, so a worker on a tight poll can read the goal one version behind and
// conflict here even though nothing about the conversation moved. Dropping the
// write would lose the whole turn this step just took (the next step would rerun
// it), so the store's shared conflict-retry policy reapplies it against a fresh
// read. Only a goal that vanished or a corrupt status ends the attempt early;
// those leave the previous checkpoint in place, which the executor is documented
// to tolerate (crash-resume path).
func (w *Worker) persistCheckpoint(ctx context.Context, r resource.Resource, checkpoint json.RawMessage) {
	_, _ = resource.UpdateByID(ctx, w.store, r.ID, func(fresh *resource.Resource) error {
		status, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		status.Checkpoint = checkpoint
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
}

// markWaiting stamps the goal's status with when its step reported it is waiting,
// so the reconciler parks the goal instead of counting the step and dispatching
// another. Like persistCheckpoint it applies the mark against a fresh read under
// the shared conflict-retry policy rather than blind-writing the claim-time record:
// the reconciler records the step's in-flight reservation on its own version around
// the same time (the job is enqueued before that write lands), so a blind Put here
// would lose that race, drop the wait mark, and let the reconciler count the wait as
// a spent step, which is the false-stall this mark exists to prevent. Only the
// waiting field is written; every reconciler-owned field is carried over from fresh.
func (w *Worker) markWaiting(ctx context.Context, r resource.Resource) {
	now := w.clk.Now()
	_, _ = resource.UpdateByID(ctx, w.store, r.ID, func(fresh *resource.Resource) error {
		status, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		status.WaitingSince = &now
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
}

// fail records a failed attempt. A transient cause is retried after an exponential
// backoff (the worker owns that policy, since the queue does not back off on its
// own); any other class (terminal, forbidden, budget, cancelled) is not retryable,
// so the step fails permanently at once rather than burning its whole attempt
// budget on a call that cannot succeed. That is what makes a down model or a bad
// API key surface as a stalled goal in seconds instead of after every retry.
func (w *Worker) fail(ctx context.Context, job jobs.Job, cause error) error {
	if fault.Classify(cause) != fault.Transient {
		return w.jobs.Fail(ctx, job.ID, cause.Error(), -1)
	}
	delay := jobs.Backoff(job.Attempt, int64(w.retryBase), int64(w.retryCeil))
	retryAt := w.clk.Now().UnixNano() + delay
	return w.jobs.Fail(ctx, job.ID, cause.Error(), retryAt)
}

func (w *Worker) signal(ctx context.Context, r resource.Resource) {
	if w.bus == nil {
		return
	}
	_ = w.bus.Publish(ctx, bus.Message{Subject: StepSubject, Payload: []byte(r.ID)})
}
