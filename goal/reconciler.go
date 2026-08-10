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
	store    resource.Store
	jobs     jobs.Queue
	clk      clock.Clock
	stop     StopEvaluator
	cleaner  Cleaner
	bus      bus.Bus       // optional; nil disables owner wake signals
	progress ProgressProbe // optional; nil disables no-progress detection
	window   WindowSource  // optional; nil leaves the plan-window share axis unbounded
	evidence Evidence      // optional; set with gate by WithLedgerGate
	gate     *EvidenceGate // optional; set with evidence by WithLedgerGate
	units    UnitSpawner   // optional; a goal carrying a unit graph without one stalls
	// auditor rules on the goal's invariants. Optional: with none wired a goal's terms
	// are carried, admitted and protected against being relaxed, but never checked,
	// which is the honest behaviour for a host that has not supplied an auditor.
	auditor InvariantAuditor
	// refusals reads the gates that refused this run. Optional: with none wired a run's
	// refusals are still recorded on the spine, they are just never read as a verdict.
	refusals RefusalProbe
	// ledgerConverge makes an unsettled ledger refuse a completion claim. It is
	// deliberately separate from having the loop wired at all: the producer runs first
	// and this follows once items are seen flipping to proven (see WithLedgerConvergence).
	ledgerConverge bool
	planning       bool
	poll           time.Duration
	waitRecheck    time.Duration // 0 derives DefaultWaitRecheckFactor * poll
	stepTries      int
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
	status.stall("ReconcileFailed", "reconcile failed terminally: "+msg, g.clk.Now())
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
	if head.ObservedSpecHash == specHash && head.settled() {
		return reconcile.Result{}, nil
	}

	// Past the skip: this reconcile is going to act, so decode the full status now
	// (InFlight, Checkpoint, WaitingSince) from the freshest record.
	status, err := DecodeStatus(r)
	if err != nil {
		return reconcile.Result{}, fault.Wrap(fault.Terminal, "goal_status_decode", err)
	}

	// Admit the two records this reconcile is about to act on, and refuse either if it
	// has been edited under the run.
	if err := admit(spec, &status); err != nil {
		return reconcile.Result{}, err
	}

	// Observe an in-flight step.
	obs, res, handled, err := g.observeInFlight(ctx, r, &status, specHash)
	if handled {
		return res, err
	}

	// The terms of the run, checked against the step that just finished and before the
	// goal parks, plans more work, fans out, settles its ledger or is judged done. A
	// broken term settles the goal from here, so nothing below it can be traded against
	// it: not the stop evaluator's verdict, and not a wait on children either.
	if res, handled, err := g.auditInvariants(ctx, r, spec, &status, specHash, obs.completed); handled {
		return res, err
	}

	// The gates that refused this run, read as a verdict about it. It sits with the audit
	// and above everything else for the same reason: a run that kept pushing on one gate
	// is not a run to judge on whether it finished, and the case this exists for is
	// exactly the run that finished by the route it was refused.
	if res, handled, err := g.checkRefusals(ctx, r, spec, &status, specHash, obs.completed); handled {
		return res, err
	}

	// A parked goal: its last step reported it is waiting on external state (a
	// fan-out's children). Do not dispatch a re-check, evaluate the stop condition,
	// or touch the budget; a settling child clears the park and signals (prompt),
	// and the recheck fallback below makes the re-check certain if that wake is
	// lost. This is what keeps a wait O(child state-changes) instead of a full
	// durable step per poll cycle.
	if status.WaitingSince != nil {
		if wait := status.WaitingSince.Add(g.recheckAfter()).Sub(g.clk.Now()); wait > 0 {
			if obs.completed {
				status.SetCondition(Condition{Type: CondReconciling, Status: "True", Reason: "AwaitingChildren", Message: "waiting on child goals"}, g.clk.Now())
				if err := g.persistStatus(ctx, r, status, specHash); err != nil {
					return reconcile.Result{}, err
				}
			}
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		status.WaitingSince = nil // fallback elapsed with no wake: re-check now
	}

	// Planning gate: a goal that plans has to have a ledger before it builds anything.
	if res, handled, err := g.planGate(ctx, r, spec, status, specHash); handled {
		return res, err
	}

	// Unit graph: while a goal's fan-out has units outstanding, the goal admits rather
	// than builds, and it returns from here. Everything below (the ledger settle, the
	// stop evaluator, the step dispatch) is what a goal does once its graph is settled,
	// so a goal cannot be judged done over a graph that is not.
	if res, handled, err := g.advanceUnits(ctx, r, spec, &status, specHash); handled {
		return res, err
	}

	// Settle the ledger against the run's own record: every unproven item the evidence
	// gate admits flips to proven here, consuming the verification that proved it. This
	// is the only path to a proven item on the run path, and it reads the durable record
	// rather than trusting a claim, so the per-item state is a projection of the spine
	// instead of a second opinion about it.
	recorded, err := g.settleLedger(ctx, r, &status)
	if err != nil {
		return reconcile.Result{}, err // classified by the record; a transient read retries
	}

	// Converged?
	met, reason, err := g.stop.Met(ctx, spec, status)
	if err != nil {
		return reconcile.Result{}, err // classified by the evaluator; transient retries
	}
	// The ledger, not the final answer, decides whether a planned goal is done. A model
	// reporting completion over unproven items is the failure mode the whole record exists
	// to catch, so the claim is held against the record: if the current item's check has
	// not been run since the last build step, run it before judging the claim; if it has,
	// and items are still unproven, the goal settles as stalled naming each one and why.
	//
	// An unplanned goal, or one whose ledger is empty, is untouched by all of this:
	// LedgerSettled is false for an empty ledger, so without this guard a goal that never
	// planned anything could never converge.
	if met && g.holdsClaimAgainstLedger(spec, status) {
		if status.VerifyPending {
			met = false // the claim has not been tested yet; verify below, then judge it
		} else {
			g.refuseCompletion(&status, recorded)
			return g.terminal(ctx, r, status, specHash)
		}
	}
	// Fold this cycle's refusal into the non-convergence count. It sits here because this
	// is the first point where both halves of the refusal are current: the evaluator has
	// just spoken, and under the ledger gate the item's check has just been settled, so
	// the feedback describes the cycle that ended rather than the one before it.
	if !met && g.countsAsCycle(obs.completed, obs.kind, status) {
		status.ObserveVerdict(reason, status.ExecutedFeedback(spec.Ledger, recorded))
	}
	if met {
		status.Phase = PhaseConverged
		status.Message = reason
		status.SetCondition(Condition{Type: CondReady, Status: "True", Reason: "StopConditionMet", Message: reason}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Converged"}, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// The goal has not converged. Ask whether it must stop anyway, and settle it under the
	// first reason that says so.
	stallReason, stallMessage, err := g.stopGuard(ctx, r, spec, status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if stallReason != "" {
		status.stall(stallReason, stallMessage, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// Dispatch the next step and record it in flight. Under the ledger gate a build step
	// is followed by the current item's declared check, so exactly one verification runs
	// per build step rather than one per reconcile tick.
	kind, reason := g.nextJobKind(spec, &status)
	return g.dispatch(ctx, r, status, specHash, kind, PhaseRunning, reason)
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
