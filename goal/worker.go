package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// StepQueue holds only goal steps, so a step worker claims nothing else.
const StepQueue = "goal-steps"

// StepSubject wakes the reconciler on step completion; the resync is the fallback.
const StepSubject = "goal.step.done"

// DefaultLease is how long a silent worker holds a step before it is re-leased, which is
// the crash-recovery path.
const DefaultLease = 5 * time.Minute

// DefaultRetryBase doubles per retry up to DefaultRetryCeiling, because retrying with no
// delay would spend the attempt budget in microseconds against the model and tools.
const (
	DefaultRetryBase    = 2 * time.Second
	DefaultRetryCeiling = 5 * time.Minute
)

// StepExecutor runs one step and returns the checkpoint to persist. It must be safe to
// re-run, because a re-leased step calls Execute again with the last checkpoint.
type StepExecutor interface {
	Execute(ctx context.Context, goal resource.Resource) (checkpoint json.RawMessage, err error)
}

// Planner runs once before building, as its own prompt, because a step also trying to
// make progress plans badly. No items is recorded as planned and the goal stalls with a
// reason, instead of retrying.
type Planner interface {
	Plan(ctx context.Context, goal resource.Resource) ([]LedgerItem, error)
}

// ErrWaiting parks the goal without counting a step, because otherwise a long wait on
// children spends the step budget and false-stalls it.
var ErrWaiting = errors.New("goal: step waiting on external state")

// Worker runs the steps the [Reconciler] dispatches and signals their completion.
type Worker struct {
	store     resource.Store
	jobs      jobs.Queue
	exec      StepExecutor
	planner   Planner // nil refuses a plan job
	verifier  ItemVerifier
	evidence  Evidence // nil refuses a verify job
	clk       clock.Timing
	bus       bus.Bus
	lease     time.Duration
	retryBase time.Duration
	retryCeil time.Duration
	// checkDir empty disables the [ShadowedCommands] search.
	checkDir string
	// claims limits which jobs are leased; nil takes every job on the queue.
	claims      jobs.Filter
	confinement *Confinement
}

// WorkerOption configures a Worker.
type WorkerOption func(*Worker)

// WithBus sets the bus a worker publishes step-completion signals on.
func WithBus(b bus.Bus) WorkerOption { return func(w *Worker) { w.bus = b } }

// WithPlanner pairs with the reconciler's [WithPlanning].
func WithPlanner(p Planner) WorkerOption { return func(w *Worker) { w.planner = p } }

// WithItemVerification takes both halves in one option, because either alone produces no
// evidence a gate sees. It pairs with the reconciler's [WithLedgerGate].
func WithItemVerification(v ItemVerifier, e Evidence) WorkerOption {
	return func(w *Worker) {
		w.verifier = v
		w.evidence = e
	}
}

// WithCheckDir names the check workspace. A check whose command a file there answers
// (test.bat for `test`) is refused unrun, because a check the agent can write measures
// nothing.
func WithCheckDir(dir string) WorkerOption { return func(w *Worker) { w.checkDir = dir } }

// WithClaimFilter limits the worker to the jobs f accepts, for a process sharing its queue
// with others whose steps it must not run.
func WithClaimFilter(f jobs.Filter) WorkerOption { return func(w *Worker) { w.claims = f } }

// WithConfinement declares where the worker's executor runs and what it may do. A step runs
// only if c covers what its goal recorded, so a worker without it runs only goals that
// recorded nothing.
func WithConfinement(c *Confinement) WorkerOption { return func(w *Worker) { w.confinement = c } }

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

// NewWorker needs the queue's clock, so a failed step's RunAt compares with claim time.
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

// ProcessOnce runs at most one step and reports whether it did.
func (w *Worker) ProcessOnce(ctx context.Context) (bool, error) {
	claimed, err := w.jobs.Claim(ctx, jobs.ClaimParams{Queue: StepQueue, Limit: 1, LeaseFor: int64(w.lease), Filter: w.claims})
	if err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	if w.claims != nil && !w.claims.Accepts(claimed[0]) {
		// A queue that ignores the filter handed over another process's step. It is not
		// run or settled, so its lease lapses; the claim still counted an attempt.
		return false, nil
	}
	return true, w.runStep(ctx, claimed[0])
}

// Run wakes on [jobs.Waker] when the queue has it. The poll stays, because it is the only
// wake for scheduled RunAt, expired leases and enqueues from other processes.
func (w *Worker) Run(ctx context.Context, poll time.Duration) {
	var ready <-chan struct{} // nil blocks forever
	if wk, ok := w.jobs.(jobs.Waker); ok {
		ready = wk.Ready()
	}
	// One timer, started stopped; branches that skip it stop and drain it so the next
	// Reset starts clean.
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

// runStep completes a vanished or terminating goal's job unrun, so a pending step does
// not keep a deleting goal alive.
func (w *Worker) runStep(ctx context.Context, job jobs.Job) error {
	r, err := w.store.GetByID(ctx, string(job.Payload))
	if errors.Is(err, resource.ErrNotFound) {
		return w.jobs.Complete(ctx, job.ID)
	}
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if r.DeletionTimestamp != nil {
		return w.jobs.Complete(ctx, job.ID)
	}
	if err := CheckConfinement(w.confinement, r); err != nil {
		return w.fail(ctx, job, err)
	}
	switch job.Kind {
	case PlanJobKind:
		return w.runPlan(ctx, job, r)
	case VerifyJobKind:
		return w.runVerify(ctx, job, r)
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
		w.persistCheckpoint(ctx, r, checkpoint) // best effort
	}
	if err := w.jobs.Complete(ctx, job.ID); err != nil {
		return err // the lease lapses and the step re-runs
	}
	w.signal(ctx, r)
	return nil
}

// runPlan fails the job on a failed ledger write, because an unplanned goal is
// re-planned forever; failing puts it on the retry ladder toward a stall.
func (w *Worker) runPlan(ctx context.Context, job jobs.Job, r resource.Resource) error {
	if w.planner == nil {
		// Terminal, because no retry supplies a missing planner.
		return w.fail(ctx, job, fault.Wrap(fault.Terminal, "goal_no_planner", errors.New("goal: plan step dispatched to a worker with no planner")))
	}
	items, err := w.planner.Plan(ctx, r)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if err := w.recordPlan(ctx, r, items); err != nil {
		return w.fail(ctx, job, err)
	}
	if err := w.jobs.Complete(ctx, job.ID); err != nil {
		return err // the lease lapses and planning re-runs
	}
	w.signal(ctx, r)
	return nil
}

// runVerify records the verdict and never marks the item proven, so the producer has no
// path around the evidence gate. A failed evidence write fails the job, because a lost
// verdict is a check run for nothing.
func (w *Worker) runVerify(ctx context.Context, job jobs.Job, r resource.Resource) error {
	if w.verifier == nil || w.evidence == nil {
		// Terminal, because otherwise the goal never converges and nothing says why.
		return w.fail(ctx, job, fault.Wrap(fault.Terminal, "goal_no_verifier",
			errors.New("goal: verify step dispatched to a worker with no item verifier")))
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return w.fail(ctx, job, fault.Wrap(fault.Terminal, "goal_spec_decode", err))
	}
	status, err := DecodeStatus(r)
	if err != nil {
		return w.fail(ctx, job, fault.Wrap(fault.Terminal, "goal_status_decode", err))
	}
	item, ok := status.CurrentItem(spec.Ledger)
	if !ok {
		// The ledger settled before this job ran.
		return w.jobs.Complete(ctx, job.ID)
	}

	verdict, err := w.verifyItem(ctx, r, item)
	if err != nil {
		return w.fail(ctx, job, err)
	}
	if _, err := w.evidence.Record(ctx, r, item.ID, verdict); err != nil {
		return w.fail(ctx, job, err)
	}
	w.markItemFeedback(ctx, r, verdict)

	if err := w.jobs.Complete(ctx, job.ID); err != nil {
		return err // the lease lapses and the check re-runs
	}
	w.signal(ctx, r)
	return nil
}

// verifyItem reports a missing program as not run, so the host's gap never counts as the
// work failing.
func (w *Worker) verifyItem(ctx context.Context, r resource.Resource, item LedgerItem) (ItemVerdict, error) {
	if shadowed := ShadowedCommands(item.Verify, w.checkDir); len(shadowed) > 0 {
		return ItemVerdict{Detail: fmt.Sprintf("the check was not run: the workspace has a file named %s, which would answer to the check's command in place of the program. "+
			"A check is not yours to rewrite, and a stand-in for its command is tampering with it. Delete the file and make the work pass the check as written.",
			strings.Join(shadowed, ", "))}, nil
	}
	verdict, err := w.verifier.VerifyItem(ctx, r, item)
	if err != nil {
		return ItemVerdict{}, err
	}
	return classifyUnrunnable(item.Verify, verdict), nil
}

// markItemFeedback hands the next build step its check's output; a pass clears it. It is
// best effort, because the verdict is already durable on the spine.
func (w *Worker) markItemFeedback(ctx context.Context, r resource.Resource, v ItemVerdict) {
	_, _ = resource.UpdateByID(ctx, w.store, r.ID, func(fresh *resource.Resource) error {
		status, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		if v.Passed {
			status.ItemFeedback = ""
		} else {
			status.ItemFeedback = v.Detail
		}
		status.observeMissing(v.Missing)
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
}

// recordPlan appends through [PlanExtension] on a fresh read, so a re-run after a crash
// adds to the ledger and a verbatim re-proposal is a no-op.
func (w *Worker) recordPlan(ctx context.Context, r resource.Resource, items []LedgerItem) error {
	_, err := resource.UpdateByID(ctx, w.store, r.ID, func(fresh *resource.Resource) error {
		spec, err := DecodeSpec(*fresh)
		if err != nil {
			return err
		}
		status, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		ledger, err := PlanExtension(spec.Ledger, items...)
		if err != nil {
			return fault.Wrap(fault.Terminal, "goal_plan_invalid", err)
		}
		if err := ValidateExtension(spec.Ledger, ledger); err != nil {
			return fault.Wrap(fault.Terminal, "goal_plan_invalid", err)
		}
		spec.Ledger = ledger
		encSpec, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		status.Planned = true
		status.SyncLedger(ledger)
		encStatus, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Spec = encSpec
		fresh.Status = encStatus
		return nil
	})
	return err
}

// persistCheckpoint retries a conflict on a fresh read, because the reconciler writes
// in-flight status after dispatch and dropping the write loses the turn. A vanished goal
// or corrupt status leaves the previous checkpoint, which the executor tolerates.
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

// markWaiting writes only WaitingSince on a fresh read, because a blind Put would race
// the reconciler's in-flight write, lose the mark and count the wait as a step.
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

// fail backs off only a transient cause, because the queue does not. Any other class
// fails at once, so a bad API key stalls the goal in seconds.
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
