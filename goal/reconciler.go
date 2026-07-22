package goal

import (
	"context"
	"errors"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// DefaultMaxSteps bounds how many steps a goal may spend when its spec sets no
// MaxSteps, so a goal that never converges stalls instead of burning budget
// forever.
const DefaultMaxSteps = 20

// DefaultPollInterval is how often the reconciler re-checks an in-flight step when
// it is not woken by a completion signal (the safety-net poll).
const DefaultPollInterval = 15 * time.Second

// DefaultWaitRecheckFactor scales the poll interval into the recheck fallback for
// a parked goal: a goal waiting on a fan-out is normally woken the moment a child
// settles, so the timed re-check only covers a lost wake and can be much slower
// than the in-flight poll. That gap is what makes a wait cheap: re-check jobs are
// paced by child completions, not by the poll.
const DefaultWaitRecheckFactor = 10

// StopEvaluator decides whether a goal's stop condition is satisfied given its
// desired spec and observed status. The production evaluator asks the model; tests
// supply a deterministic one. It is the agent's semantic convergence test, the
// thing a numeric controller has no equivalent for.
type StopEvaluator interface {
	Met(ctx context.Context, spec Spec, status Status) (met bool, reason string, err error)
}

// Cleaner runs the teardown a goal's finalizer guards (remove a worktree, cancel a
// run, delete child goals) before the goal is allowed to be deleted. A nil Cleaner
// means there is nothing external to clean up.
type Cleaner interface {
	Cleanup(ctx context.Context, r resource.Resource) error
}

// Reconciler drives a Goal toward its stop condition. It never runs the work
// itself: it dispatches a step to the durable job queue, records it in flight so a
// re-reconcile observes rather than relaunches, and re-evaluates when the step
// completes. That keeps each reconcile quick and idempotent, and because progress
// is recorded in status, a crash resumes mid-goal instead of restarting.
type Reconciler struct {
	store       resource.Store
	jobs        jobs.Queue
	clk         clock.Clock
	stop        StopEvaluator
	cleaner     Cleaner
	bus         bus.Bus // optional; nil disables owner wake signals
	planning    bool
	poll        time.Duration
	waitRecheck time.Duration // 0 derives DefaultWaitRecheckFactor * poll
	stepTries   int
}

// Option configures a Reconciler.
type Option func(*Reconciler)

// WithCleaner sets the teardown hook run before a goal's finalizer is removed.
func WithCleaner(c Cleaner) Option { return func(g *Reconciler) { g.cleaner = c } }

// WithPollInterval overrides the in-flight re-check interval.
func WithPollInterval(d time.Duration) Option {
	return func(g *Reconciler) {
		if d > 0 {
			g.poll = d
		}
	}
}

// WithWaitRecheck overrides how long a parked goal (waiting on a fan-out's
// children) may sit before the reconciler re-checks it without a wake signal.
func WithWaitRecheck(d time.Duration) Option {
	return func(g *Reconciler) {
		if d > 0 {
			g.waitRecheck = d
		}
	}
}

// WithPlanning makes the goal plan before it builds: the first thing a goal does is
// a planning step that expands its objective into a ledger, and no build step is
// dispatched until that ledger exists. It pairs with a Worker configured with a
// Planner, which is what actually runs the planning step.
//
// It is an option rather than the unconditional behaviour because a goal composed
// without a planner has no way to produce a ledger, and gating those goals would
// park every one of them forever. Wiring the planner and turning this on are the
// same decision, made in the same place.
func WithPlanning() Option { return func(g *Reconciler) { g.planning = true } }

// WithWakeBus sets the bus the reconciler signals a parked owner on when one of
// its children settles, so a fan-out parent re-checks on child state-change
// instead of waiting out the recheck fallback.
func WithWakeBus(b bus.Bus) Option { return func(g *Reconciler) { g.bus = b } }

// WithStepMaxAttempts bounds how many times a single dispatched step is retried by
// the job queue before it goes dead and stalls the goal (0 uses the queue default).
func WithStepMaxAttempts(n int) Option {
	return func(g *Reconciler) {
		if n > 0 {
			g.stepTries = n
		}
	}
}

// NewReconciler builds a Reconciler over the given store, job queue, clock and
// stop evaluator.
func NewReconciler(store resource.Store, q jobs.Queue, clk clock.Clock, stop StopEvaluator, opts ...Option) *Reconciler {
	g := &Reconciler{store: store, jobs: q, clk: clk, stop: stop, poll: DefaultPollInterval}
	for _, o := range opts {
		o(g)
	}
	return g
}

var _ reconcile.Reconciler[reconcile.Ref] = (*Reconciler)(nil)

// Reconcile drives one goal one level-triggered step toward its desired state.
//
// A reconcile that fails terminally is not merely dropped. fault.Classify calls an
// unclassified error Terminal, and the controller does not retry a terminal error,
// so a goal whose reconcile can never succeed (a spec it cannot decode, a stop
// evaluator that hard-fails) would otherwise sit non-terminal forever: no step in
// flight, nothing to wake it, and a caller waiting on a phase that will never
// arrive. A goal that cannot be reconciled has not paused, it has failed, so it is
// recorded as stalled with the cause. Retryable classes are returned untouched:
// transient errors back off and retry, and a cancellation is shutdown, not failure.
func (g *Reconciler) Reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
	res, err := g.reconcile(ctx, ref)
	if err == nil || fault.Classify(err) != fault.Terminal {
		return res, err
	}
	if serr := g.stall(ctx, ref, err); serr != nil {
		// The stall write itself failed (a conflict is transient): retry, do not lose
		// the cause.
		return reconcile.Result{}, serr
	}
	return reconcile.Result{}, nil
}

// stall records a terminally-failed reconcile on the goal, so the failure surfaces
// as a settled phase a caller can observe instead of an unbounded wait. A goal that
// has since gone or already settled is left as it is.
func (g *Reconciler) stall(ctx context.Context, ref reconcile.Ref, cause error) error {
	r, err := g.store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if errors.Is(err, resource.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	status, err := DecodeStatus(r)
	if err != nil {
		// The status is what could not be decoded. Stalling still has to work, so
		// start from an empty one rather than failing in the failure path.
		status = Status{}
	}
	if status.Phase == PhaseConverged || status.Phase == PhaseStalled {
		return nil
	}
	msg := cause.Error()
	status.InFlight = nil
	status.WaitingSince = nil
	status.Phase = PhaseStalled
	status.Message = "reconcile failed terminally: " + msg
	status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "ReconcileFailed", Message: msg}, g.clk.Now())
	status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "ReconcileFailed"}, g.clk.Now())
	_, err = g.terminal(ctx, r, status, r.SpecHash)
	return err
}

// reconcile is the reconcile proper; Reconcile wraps it to settle terminal faults.
func (g *Reconciler) reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
	r, err := g.store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if errors.Is(err, resource.ErrNotFound) {
		return reconcile.Result{}, nil // already gone
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	spec, err := DecodeSpec(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_spec_decode", err)
	}
	// Decode only the scalar status head first: the no-op skip below needs the phase
	// and observed spec hash, not the opaque Checkpoint (the whole transcript). A
	// settled goal's periodic resync short-circuits at the skip without ever copying
	// the transcript. The full status is decoded once the reconcile is going to act.
	head, err := decodeStatusHead(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	// Deletion: run cleanup, then drop our finalizer so the delete can complete.
	if r.DeletionTimestamp != nil {
		return g.finalize(ctx, r)
	}

	// Garbage-collect an orphan: if a controller owner is gone or terminating, this
	// goal belongs to a subtree being torn down, so request its own deletion. Its
	// finalizer then runs cleanup and the reap cascades down the tree. Owner liveness
	// is resolved by id; the resync re-checks it if an owner vanishes between
	// reconciles. A root goal (no controller owner) is never orphaned.
	if gone, err := resource.OwnerGone(ctx, g.store, r); err != nil {
		return reconcile.Result{}, err
	} else if gone {
		if err := g.store.Delete(ctx, r.Kind, r.Scope, r.Name); err != nil {
			return reconcile.Result{}, putErr(err)
		}
		return reconcile.Result{}, nil
	}

	// Ensure our finalizer is present before doing anything that creates state we
	// must later clean up, then continue in the same pass using the freshly stamped
	// record. Returning here instead would leave the goal idle until the next
	// resync, because a self-write does not re-trigger a reconcile on its own.
	if !hasFinalizer(r.Finalizers, Finalizer) {
		r.Finalizers = append(r.Finalizers, Finalizer)
		updated, err := g.store.Put(ctx, r)
		if err != nil {
			return reconcile.Result{}, putErr(err)
		}
		r = updated
	}

	// The Stamper stamps SpecHash on every write, so the record carries it and the
	// no-op check reads a field instead of re-canonicalizing the spec each tick.
	specHash := r.SpecHash
	// No-op skip: spec unchanged and the goal has already settled.
	if head.ObservedSpecHash == specHash && (head.Phase == PhaseConverged || head.Phase == PhaseStalled) {
		return reconcile.Result{}, nil
	}

	// Past the skip: this reconcile is going to act, so decode the full status now
	// (InFlight, Checkpoint, WaitingSince) from the freshest record.
	status, err := DecodeStatus(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	// A ledger that lost or rewrote an item between reconciles is the definition of
	// done being edited mid-run, so the goal fails rather than adopting the edit.
	if err := status.ValidateLedger(spec.Ledger); err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_ledger_regressed", err)
	}
	status.SyncLedger(spec.Ledger)

	// Observe an in-flight step.
	observed := false
	if status.InFlight != nil {
		inFlightKind := status.InFlight.Kind
		job, err := g.jobs.Get(ctx, status.InFlight.JobID)
		switch {
		case err != nil:
			// The job record is gone; treat the step as lost and retry from clean.
			status.InFlight = nil
		case job.State == jobs.StateRunning || job.State == jobs.StatePending:
			return reconcile.Result{RequeueAfter: g.poll}, nil // still working
		case job.State == jobs.StateDead:
			status.InFlight = nil
			status.Phase = PhaseStalled
			status.Message = "step failed: " + job.LastError
			status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "StepFailed", Message: job.LastError}, g.clk.Now())
			status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "StepFailed"}, g.clk.Now())
			return g.terminal(ctx, r, status, specHash)
		default: // StateDone: a step completed.
			status.InFlight = nil
			observed = true
			// A step that parked the goal (ErrWaiting) made no progress, so it
			// does not count against the step budget: a fan-out whose children
			// outlast the budget's worth of re-checks must wait, not false-stall.
			// A planning step is not building either: it is the phase that decides
			// what the build budget will be spent on, so charging the budget for it
			// would make a goal that plans strictly poorer than one that does not.
			if status.WaitingSince == nil && inFlightKind != PlanJobKind {
				status.Steps++
			}
		}
	}

	// A parked goal: its last step reported it is waiting on external state (a
	// fan-out's children). Do not dispatch a re-check, evaluate the stop condition,
	// or touch the budget; a settling child clears the park and signals (prompt),
	// and the recheck fallback below makes the re-check certain if that wake is
	// lost. This is what keeps a wait O(child state-changes) instead of a full
	// durable step per poll cycle.
	if status.WaitingSince != nil {
		if wait := status.WaitingSince.Add(g.recheckAfter()).Sub(g.clk.Now()); wait > 0 {
			if observed {
				status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: "AwaitingChildren", Message: "waiting on child goals"}, g.clk.Now())
				if err := g.persistStatus(ctx, r, status, specHash); err != nil {
					return reconcile.Result{}, err
				}
			}
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		status.WaitingSince = nil // fallback elapsed with no wake: re-check now
	}

	// Planning gate. A goal that plans expands its objective into a ledger before it
	// builds anything, so the first dispatch is a planning step and the stop
	// condition is not evaluated until there is a record to evaluate it against.
	if g.planning && !status.Planned {
		return g.dispatch(ctx, r, status, specHash, PlanJobKind, PhasePlanning, "PlanDispatched")
	}
	// A planner that ran and produced nothing leaves a goal with no definition of
	// done, which is a stall. Letting it build anyway is how a run ends up claiming
	// success against a record that never said what success was.
	if g.planning && len(spec.Ledger) == 0 {
		status.Phase = PhaseStalled
		status.Message = "planning produced an empty ledger"
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "EmptyLedger", Message: status.Message}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// Converged?
	met, reason, err := g.stop.Met(ctx, spec, status)
	if err != nil {
		return reconcile.Result{}, err // classified by the evaluator; transient retries
	}
	if met {
		status.Phase = PhaseConverged
		status.Message = reason
		status.SetCondition(Condition{Type: CondReady, Status: "True", Reason: "StopConditionMet", Message: reason}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Converged"}, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// Budget guard.
	if status.Steps >= maxSteps(spec) {
		status.Phase = PhaseStalled
		status.Message = "step budget exhausted before the stop condition was met"
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "BudgetExhausted", Message: status.Message}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// Dispatch the next step and record it in flight.
	return g.dispatch(ctx, r, status, specHash, StepJobKind, PhaseRunning, "StepDispatched")
}

// dispatch enqueues one job of the given kind against the goal and records it in
// flight under the phase that job puts the goal in. Planning and building share it
// so a planning step gets the same reservation, lease, crash-resume and retry
// behaviour a build step has, and the only thing that differs between them is what
// the executor is asked to do.
func (g *Reconciler) dispatch(ctx context.Context, r resource.Resource, status Status, specHash, kind string, phase Phase, reason string) (reconcile.Result, error) {
	job, err := g.jobs.Enqueue(ctx, jobs.EnqueueParams{
		Queue:       StepQueue,
		Kind:        kind,
		Payload:     []byte(r.ID),
		Scope:       state.Scope(r.Scope),
		MaxAttempts: g.stepTries,
	})
	if err != nil {
		return reconcile.Result{}, putErr(err)
	}
	status.Phase = phase
	status.InFlight = &InFlight{JobID: job.ID, StartedAt: g.clk.Now(), Kind: kind}
	status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: reason}, g.clk.Now())
	if err := g.recordDispatch(ctx, r, status, specHash); err != nil {
		return reconcile.Result{}, err
	}
	// Re-check the dispatched step after poll even if its completion signal is
	// lost: the worker's bus signal makes observation prompt, this makes it certain.
	return reconcile.Result{RequeueAfter: g.poll}, nil
}

// finalize runs cleanup once and then removes our finalizer, letting the store
// complete the deletion. If cleanup fails the finalizer stays, so the goal remains
// terminating and the delete is retried, never leaking the owned state.
func (g *Reconciler) finalize(ctx context.Context, r resource.Resource) (reconcile.Result, error) {
	if !hasFinalizer(r.Finalizers, Finalizer) {
		return reconcile.Result{}, nil // ours already cleared; nothing to do
	}
	if g.cleaner != nil {
		if err := g.cleaner.Cleanup(ctx, r); err != nil {
			// Retry; do not drop the finalizer. Cleanup is retried by design (that is
			// what stops the owned state from leaking), so the failure is transient
			// whatever the cleaner says: an unclassified error would otherwise be read
			// as terminal and abandoned.
			return reconcile.Result{}, fault.Wrap(fault.Transient, "goal_cleanup", err)
		}
	}
	r.Finalizers = removeFinalizer(r.Finalizers, Finalizer)
	if _, err := g.store.Put(ctx, r); err != nil {
		return reconcile.Result{}, putErr(err)
	}
	return reconcile.Result{}, nil
}

// recordDispatch persists the in-flight reservation for a step that was just
// enqueued. Unlike a settled-status write, it must survive a race with the step's
// own worker: the job is enqueued (Enqueue, above) before this write runs, so a
// worker on a tight poll can claim it, take its turn, and persist that turn's
// checkpoint before this write lands. A blind Put would then lose the optimistic
// race and be dropped, and the dropped InFlight marker means the completed step is
// never observed in the next pass and never counted against the step budget, so an
// extra turn runs past MaxSteps (the goal converges where it must stall). Retrying
// the whole reconcile does not recover it: the retry re-reads a state that has
// already lost the job-to-reservation link. So this reapplies the reservation onto
// a fresh read with the shared conflict-retry policy instead. Only reconciler-owned
// fields are written; the worker-owned checkpoint and waiting mark are carried over
// from the fresh record so neither writer clobbers the other.
func (g *Reconciler) recordDispatch(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	status.ObservedSpecHash = specHash
	_, err := resource.UpdateByID(ctx, g.store, r.ID, func(fresh *resource.Resource) error {
		cur, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		status.Checkpoint = cur.Checkpoint
		status.WaitingSince = cur.WaitingSince
		// The planning mark and the per-item state are the worker's too, and the
		// planning step is the one most likely to land inside this window: the plan
		// job is enqueued before this reservation is written, so a worker that
		// claims and finishes it first would otherwise have its ledger erased here
		// and be asked to plan the same goal all over again.
		status.Planned = cur.Planned
		status.Ledger = cur.Ledger
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
	if err != nil {
		return putErr(err)
	}
	return nil
}

// persistStatus records the observed spec hash and persists the status via the
// store's optimistic-concurrency Put.
func (g *Reconciler) persistStatus(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	status.ObservedSpecHash = specHash
	enc, err := status.Encode()
	if err != nil {
		return fault.Wrap(fault.Terminal, "goal_status_encode", err)
	}
	r.Status = enc
	if _, err := g.store.Put(ctx, r); err != nil {
		return putErr(err)
	}
	return nil
}

// terminal persists a settled status (converged or stalled) and requests no
// requeue: the goal has reached a steady state, so it is only revisited when its
// spec changes or at the next resync, not on a timer.
func (g *Reconciler) terminal(ctx context.Context, r resource.Resource, status Status, specHash string) (reconcile.Result, error) {
	if err := g.persistStatus(ctx, r, status, specHash); err != nil {
		return reconcile.Result{}, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, nil
}

// recheckAfter is how long a parked goal waits before re-checking without a wake.
func (g *Reconciler) recheckAfter() time.Duration {
	if g.waitRecheck > 0 {
		return g.waitRecheck
	}
	return DefaultWaitRecheckFactor * g.poll
}

// wakeOwner clears the controller owner's park and signals it, so a parent goal
// waiting on a fan-out re-checks its children the moment one settles instead of
// waiting out the recheck fallback. Best effort: a lost wake costs latency (the
// fallback catches it), never correctness, so every failure here is dropped. The
// conflict retry matters because the parent's status is also written by its own
// reconciler and worker; losing every race would silently downgrade the wake.
func (g *Reconciler) wakeOwner(ctx context.Context, r resource.Resource) {
	owner, ok := r.Controller()
	if !ok || owner.Kind != Kind {
		return
	}
	if _, err := resource.UpdateByID(ctx, g.store, owner.ID, func(o *resource.Resource) error {
		status, err := DecodeStatus(*o)
		if err != nil || status.WaitingSince == nil {
			return resource.ErrSkipUpdate // not parked; the signal alone suffices
		}
		status.WaitingSince = nil
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		o.Status = enc
		return nil
	}); err != nil && !errors.Is(err, resource.ErrConflict) {
		return // owner gone or unreadable; nothing to wake
	}
	if g.bus != nil {
		_ = g.bus.Publish(ctx, bus.Message{Subject: StepSubject, Payload: []byte(owner.ID)})
	}
}

func maxSteps(s Spec) int {
	if s.MaxSteps > 0 {
		return s.MaxSteps
	}
	return DefaultMaxSteps
}

// putErr maps a write conflict to a Transient error so the controller backs off
// and retries with a fresh read, rather than treating a lost race as fatal.
func putErr(err error) error {
	if errors.Is(err, resource.ErrConflict) {
		return fault.Wrap(fault.Transient, "goal_write_conflict", err)
	}
	return err
}
