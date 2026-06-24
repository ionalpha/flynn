package goal

import (
	"context"
	"errors"
	"time"

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
	store     resource.Store
	jobs      jobs.Queue
	clk       clock.Clock
	stop      StopEvaluator
	cleaner   Cleaner
	poll      time.Duration
	stepTries int
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
func (g *Reconciler) Reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
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
	status, err := DecodeStatus(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	// Deletion: run cleanup, then drop our finalizer so the delete can complete.
	if r.DeletionTimestamp != nil {
		return g.finalize(ctx, r)
	}

	// Ensure our finalizer is present before doing anything that creates state we
	// must later clean up. Adding it is one write; we reconcile again afterwards.
	if !hasFinalizer(r.Finalizers, Finalizer) {
		r.Finalizers = append(r.Finalizers, Finalizer)
		if _, err := g.store.Put(ctx, r); err != nil {
			return reconcile.Result{}, putErr(err)
		}
		return reconcile.Result{}, nil
	}

	specHash, err := resource.SpecHash(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_spec_hash", err)
	}
	// No-op skip: spec unchanged and the goal has already settled.
	if status.ObservedSpecHash == specHash && (status.Phase == PhaseConverged || status.Phase == PhaseStalled) {
		return reconcile.Result{}, nil
	}

	// Observe an in-flight step.
	if status.InFlight != nil {
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
			return g.writeStatus(ctx, r, status, specHash)
		default: // StateDone: a step completed.
			status.InFlight = nil
			status.Steps++
		}
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
		return g.writeStatus(ctx, r, status, specHash)
	}

	// Budget guard.
	if status.Steps >= maxSteps(spec) {
		status.Phase = PhaseStalled
		status.Message = "step budget exhausted before the stop condition was met"
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "BudgetExhausted", Message: status.Message}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
		return g.writeStatus(ctx, r, status, specHash)
	}

	// Dispatch the next step and record it in flight.
	job, err := g.jobs.Enqueue(ctx, jobs.EnqueueParams{
		Queue:       StepQueue,
		Kind:        StepJobKind,
		Payload:     []byte(r.ID),
		Scope:       state.Scope(r.Scope),
		MaxAttempts: g.stepTries,
	})
	if err != nil {
		return reconcile.Result{}, putErr(err)
	}
	status.Phase = PhaseRunning
	status.InFlight = &InFlight{JobID: job.ID, StartedAt: g.clk.Now()}
	status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: "StepDispatched"}, g.clk.Now())
	return g.writeStatus(ctx, r, status, specHash)
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
			return reconcile.Result{}, err // retry; do not drop the finalizer
		}
	}
	r.Finalizers = removeFinalizer(r.Finalizers, Finalizer)
	if _, err := g.store.Put(ctx, r); err != nil {
		return reconcile.Result{}, putErr(err)
	}
	return reconcile.Result{}, nil
}

// writeStatus records the observed spec hash and persists the status via the
// store's optimistic-concurrency Put.
func (g *Reconciler) writeStatus(ctx context.Context, r resource.Resource, status Status, specHash string) (reconcile.Result, error) {
	status.ObservedSpecHash = specHash
	enc, err := status.Encode()
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_encode", err)
	}
	r.Status = enc
	if _, err := g.store.Put(ctx, r); err != nil {
		return reconcile.Result{}, putErr(err)
	}
	return reconcile.Result{}, nil
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
