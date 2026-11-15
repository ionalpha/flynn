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
)

// DefaultMaxSteps applies when the spec sets no MaxSteps, so a goal that never converges
// stalls.
const DefaultMaxSteps = 20

// DefaultPollInterval re-checks an in-flight step when no completion signal arrives.
const DefaultPollInterval = 15 * time.Second

// DefaultWaitRecheckFactor multiplies the poll for a parked goal's recheck. It can be
// slow, because a settling child wakes the parent and the recheck covers only a lost wake.
const DefaultWaitRecheckFactor = 10

// StopEvaluator judges the stop condition; the production one asks the model.
type StopEvaluator interface {
	Met(ctx context.Context, spec Spec, status Status) (met bool, reason string, err error)
}

// Cleaner runs the teardown the finalizer guards before deletion. Nil means nothing to
// clean.
type Cleaner interface {
	Cleanup(ctx context.Context, r resource.Resource) error
}

// Reconciler never runs work. It dispatches a step and records it in flight, so a
// re-reconcile observes instead of relaunching and a crash resumes mid-goal.
type Reconciler struct {
	store    resource.Store
	jobs     jobs.Queue
	clk      clock.Clock
	stop     StopEvaluator
	cleaner  Cleaner
	bus      bus.Bus
	progress ProgressProbe
	window   WindowSource
	evidence Evidence      // set with gate by WithLedgerGate
	gate     *EvidenceGate // set with evidence by WithLedgerGate
	units    UnitSpawner
	auditor  InvariantAuditor
	judge    SteerJudge
	// halt nil still settles a killed goal, after the current step finishes.
	halt     Halter
	refusals RefusalProbe
	// ledgerConverge is separate from the gate, so the producer can run before claims are
	// refused ([WithLedgerConvergence]).
	ledgerConverge bool
	planning       bool
	poll           time.Duration
	waitRecheck    time.Duration // 0 derives DefaultWaitRecheckFactor * poll
	stepTries      int
}

// NewReconciler uses [DefaultPollInterval] unless an [Option] changes it.
func NewReconciler(store resource.Store, q jobs.Queue, clk clock.Clock, stop StopEvaluator, opts ...Option) *Reconciler {
	g := &Reconciler{store: store, jobs: q, clk: clk, stop: stop, poll: DefaultPollInterval}
	for _, o := range opts {
		o(g)
	}
	return g
}

var _ reconcile.Reconciler[reconcile.Ref] = (*Reconciler)(nil)

// Reconcile stalls the goal on a terminal error, because the controller drops it and the
// goal would otherwise wait forever with nothing to wake it. Other classes are returned.
func (g *Reconciler) Reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
	res, err := g.reconcile(ctx, ref)
	if err == nil || fault.Classify(err) != fault.Terminal {
		return res, err
	}
	if serr := g.stall(ctx, ref, err); serr != nil {
		// Returned so the stall is retried and the cause kept.
		return reconcile.Result{}, serr
	}
	return reconcile.Result{}, nil
}

// stall leaves a goal that is gone or already settled as it is.
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
		// Start empty, so the failure path cannot fail on the same decode.
		status = Status{}
	}
	if status.Phase == PhaseConverged || status.Phase == PhaseStalled {
		return nil
	}
	msg := cause.Error()
	status.InFlight = nil
	status.WaitingSince = nil
	status.stall("ReconcileFailed", "reconcile failed terminally: "+msg, g.clk.Now())
	_, err = g.terminal(ctx, r, status, r.SpecHash)
	return err
}

func (g *Reconciler) reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
	r, err := g.store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if errors.Is(err, resource.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	spec, err := DecodeSpec(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_spec_decode", err)
	}
	// The head is enough for the no-op skip, so a settled goal's resync never copies the
	// transcript in Checkpoint.
	head, err := decodeStatusHead(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	if r.DeletionTimestamp != nil {
		return g.finalize(ctx, r)
	}
	r, done, err := g.ensureOwned(ctx, r)
	if done || err != nil {
		return reconcile.Result{}, err
	}

	// The Stamper sets SpecHash on every write, so no tick re-canonicalizes the spec.
	specHash := r.SpecHash
	if head.ObservedSpecHash == specHash && head.settled() {
		return reconcile.Result{}, nil
	}

	status, err := DecodeStatus(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	if err := admit(spec, &status); err != nil {
		return reconcile.Result{}, err
	}
	return g.advance(ctx, r, spec, &status, specHash)
}

// ensureOwned deletes an orphan and reports done; otherwise it returns the goal carrying
// our finalizer.
func (g *Reconciler) ensureOwned(ctx context.Context, r resource.Resource) (resource.Resource, bool, error) {
	// An orphan deletes itself, so its finalizer cleans up and the reap cascades. The
	// resync re-checks an owner that vanishes between reconciles.
	if gone, err := resource.OwnerGone(ctx, g.store, r); err != nil {
		return r, true, err
	} else if gone {
		if err := g.store.Delete(ctx, r.Kind, r.Scope, r.Name); err != nil {
			return r, true, putErr(err)
		}
		return r, true, nil
	}

	// The finalizer goes on before any state needing cleanup. The pass continues, because
	// a self-write does not re-trigger a reconcile.
	if !hasFinalizer(r.Finalizers, Finalizer) {
		r.Finalizers = append(r.Finalizers, Finalizer)
		updated, err := g.store.Put(ctx, r)
		if err != nil {
			return r, true, putErr(err)
		}
		r = updated
	}
	return r, false, nil
}

// advance runs the gates in policy order, and the first to settle, park or requeue ends
// the pass, so the order is the order a run is held to account.
func (g *Reconciler) advance(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string) (reconcile.Result, error) {
	// Kill is first, because everything below asks what to do next and a killed run has
	// no next.
	if res, handled, err := g.applyKill(ctx, r, spec, status, specHash); handled {
		return res, err
	}

	obs, res, handled, err := g.observeInFlight(ctx, r, status, specHash)
	if handled {
		return res, err
	}

	// Invariants come before parking, planning, fan-out and judging, so nothing below can
	// be traded against a broken term.
	if res, handled, err := g.auditInvariants(ctx, r, spec, status, specHash, obs.completed); handled {
		return res, err
	}

	// Refusals come before judging, because the run they catch finished by the route it
	// was refused.
	if res, handled, err := g.checkRefusals(ctx, r, spec, status, specHash, obs.completed); handled {
		return res, err
	}

	if res, handled, err := g.awaitChildren(ctx, r, status, specHash, obs.completed); handled {
		return res, err
	}

	if res, handled, err := g.planGate(ctx, r, spec, *status, specHash); handled {
		return res, err
	}

	// Units come before judging, so a goal is never done over an unsettled graph.
	if res, handled, err := g.advanceUnits(ctx, r, spec, status, specHash); handled {
		return res, err
	}
	return g.judgeOrStep(ctx, r, spec, status, specHash, obs)
}

// awaitChildren spends no step or budget, so a wait costs O(child state changes). The
// recheck fallback covers a lost wake.
func (g *Reconciler) awaitChildren(ctx context.Context, r resource.Resource, status *Status, specHash string, completed bool) (reconcile.Result, bool, error) {
	if status.WaitingSince == nil {
		return reconcile.Result{}, false, nil
	}
	if wait := status.WaitingSince.Add(g.recheckAfter()).Sub(g.clk.Now()); wait > 0 {
		if completed {
			status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: "AwaitingChildren", Message: "waiting on child goals"}, g.clk.Now())
			if err := g.persistStatus(ctx, r, *status, specHash); err != nil {
				return reconcile.Result{}, true, err
			}
		}
		return reconcile.Result{RequeueAfter: wait}, true, nil
	}
	status.WaitingSince = nil // no wake came; re-check now
	return reconcile.Result{}, false, nil
}

// judgeOrStep runs once nothing has stopped or parked the goal.
func (g *Reconciler) judgeOrStep(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string, obs observation) (reconcile.Result, error) {
	// The only run-path route to a proven item, read from the durable record.
	recorded, err := g.settleLedger(ctx, r, status)
	if err != nil {
		return reconcile.Result{}, err
	}

	met, reason, err := g.stop.Met(ctx, spec, *status)
	if err != nil {
		return reconcile.Result{}, err
	}
	// A planned goal's claim is held against the ledger: an unrun check runs first, and
	// unproven items then stall the goal naming each. An empty ledger skips this, because
	// it is never settled and the goal could never converge.
	if met && g.holdsClaimAgainstLedger(spec, *status) {
		if status.VerifyPending {
			met = false // verify below, then judge
		} else {
			g.refuseCompletion(status, recorded)
			return g.terminal(ctx, r, *status, specHash)
		}
	}
	// Without required proof the claim waits for the plan to be worked through. Its wording
	// is dropped, so the non-convergence count compares check output, not varying prose.
	if met && g.claimOverOpenItem(spec, *status) {
		g.moveOffUnrunnableItem(spec, status, recorded)
		if g.claimOverOpenItem(spec, *status) {
			met, reason = false, ""
		}
	}
	// Counted here, because only now are the verdict and the item's check both from this
	// cycle.
	if !met && g.countsAsCycle(obs.completed, obs.kind, *status) {
		feedback := status.ExecutedFeedback(spec.Ledger, recorded)
		status.ObserveVerdict(reason, feedback)
		g.moveOffUnprovableItem(spec, status, feedback)
	}
	if met {
		return g.converge(ctx, r, spec, status, specHash, reason)
	}

	stallReason, stallMessage, err := g.stopGuard(ctx, r, spec, *status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if stallReason != "" {
		status.stall(stallReason, stallMessage, g.clk.Now())
		return g.terminal(ctx, r, *status, specHash)
	}

	// Under the ledger gate each build step is followed by one check, never one per tick.
	kind, reason := g.nextJobKind(spec, status)
	return g.dispatch(ctx, r, *status, specHash, kind, PhaseRunning, reason)
}

// converge checks steers last, because the run's account of finishing is what is judged.
func (g *Reconciler) converge(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash, reason string) (reconcile.Result, error) {
	if res, handled, err := g.dischargeSteers(ctx, r, spec, status, specHash, reason); handled {
		return res, err
	}
	status.Phase = PhaseConverged
	status.Message = reason
	status.SetCondition(Condition{Type: CondReady, Status: "True", Reason: "StopConditionMet", Message: reason}, g.clk.Now())
	status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Converged"}, g.clk.Now())
	return g.terminal(ctx, r, *status, specHash)
}

// finalize keeps the finalizer when cleanup fails, so the delete retries and nothing
// leaks.
func (g *Reconciler) finalize(ctx context.Context, r resource.Resource) (reconcile.Result, error) {
	if !hasFinalizer(r.Finalizers, Finalizer) {
		return reconcile.Result{}, nil
	}
	if g.cleaner != nil {
		if err := g.cleaner.Cleanup(ctx, r); err != nil {
			// Transient whatever the cleaner says, because a terminal error would abandon
			// the cleanup.
			return reconcile.Result{}, fault.Wrap(fault.Transient, "goal_cleanup", err)
		}
	}
	r.Finalizers = removeFinalizer(r.Finalizers, Finalizer)
	if _, err := g.store.Put(ctx, r); err != nil {
		return reconcile.Result{}, putErr(err)
	}
	return reconcile.Result{}, nil
}
