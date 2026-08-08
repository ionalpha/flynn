package goal

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// ledgerConverge makes an unsettled ledger refuse a completion claim. It is
	// deliberately separate from having the loop wired at all: the producer runs first
	// and this follows once items are seen flipping to proven (see WithLedgerConvergence).
	ledgerConverge bool
	planning       bool
	poll           time.Duration
	waitRecheck    time.Duration // 0 derives DefaultWaitRecheckFactor * poll
	stepTries      int
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

// WithLedgerGate runs the ledger loop: after each build step the run's current item has
// its declared check run, and the verdicts recorded on the run's durable record are folded
// back through gate to settle the ledger. Items flip to proven from the record, and only
// from the record.
//
// Until this is wired the ledger is planned, validated and protected against tampering,
// and then never asked whether the work is actually done, which is the exact failure the
// ledger exists to foreclose, a run declaring victory having written nothing. gate is the
// self-tested EvidenceGate; a nil gate or nil evidence leaves the loop open, so a goal
// behaves exactly as it did before, and pairing them is the composition's job (see
// runtime.Config).
//
// On its own this changes what the record says, not what the goal does: convergence is
// still the model's call. WithLedgerConvergence is what makes the record binding.
func WithLedgerGate(e Evidence, gate *EvidenceGate) Option {
	return func(g *Reconciler) {
		if e != nil && gate != nil {
			g.evidence, g.gate = e, gate
		}
	}
}

// WithLedgerConvergence makes the ledger, not the final answer, decide whether a planned
// goal is done: a model reporting completion with items still unproven does not converge,
// it settles as stalled naming each unproven item and why.
//
// It is separate from WithLedgerGate because the two carry different risk, and the
// staging between them is the point. Turning the refusal on before anything produces
// verifications stalls every goal, which is why the gate's original deferral was correct;
// running the producer first makes items visibly flip to proven on real runs, and this is
// then turned on against evidence rather than hope.
//
// It is emphatically not a switch that may ship permanently off. A gate that is loaded and
// does nothing is precisely the failure the gate's own self-test exists to catch, worn one
// level up, and a composition that leaves this off indefinitely has rebuilt it.
func WithLedgerConvergence() Option { return func(g *Reconciler) { g.ledgerConverge = true } }

// WithWakeBus sets the bus the reconciler signals a parked owner on when one of
// its children settles, so a fan-out parent re-checks on child state-change
// instead of waiting out the recheck fallback.
func WithWakeBus(b bus.Bus) Option { return func(g *Reconciler) { g.bus = b } }

// WithProgressProbe turns on no-progress detection: after each build step the
// reconciler asks the probe for a fingerprint of the substantive work recorded so far,
// and stops the goal once that fingerprint has not changed for NoProgressLimit
// consecutive steps (see progress.go). It is an option rather than always-on because
// the probe reads whatever durable record the runtime keeps (the spine), which a bare
// reconciler assembled without one does not have; a nil probe leaves detection off and
// a goal is bounded only by its step budget, exactly as before.
func WithProgressProbe(p ProgressProbe) Option { return func(g *Reconciler) { g.progress = p } }

// WithWindowSource wires the source the spend guard reads to enforce a goal's
// WindowFraction ceiling (share of the plan window). Flynn ships no source of its own
// because the plan window belongs to the account the host app runs under; without one
// the token and cost ceilings still apply and only the window axis is left unbounded.
func WithWindowSource(w WindowSource) Option { return func(g *Reconciler) { g.window = w } }

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
		status.Phase = PhaseStalled
		status.Message = stallMessage
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: stallReason, Message: stallMessage}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
		return g.terminal(ctx, r, status, specHash)
	}

	// Dispatch the next step and record it in flight. Under the ledger gate a build step
	// is followed by the current item's declared check, so exactly one verification runs
	// per build step rather than one per reconcile tick.
	kind, reason := g.nextJobKind(spec, &status)
	return g.dispatch(ctx, r, status, specHash, kind, PhaseRunning, reason)
}

// observation is what a reconcile learned from the step it had in flight: whether one
// completed this pass, and of what kind. Both halves are read further down the
// reconcile by the guards that only apply to a cycle in which the agent did something.
type observation struct {
	completed bool
	kind      string
}

// observeInFlight resolves the step this goal had in flight and folds its outcome into
// the status: a lost job clears the mark and retries from clean, a completed one banks
// its progress against the budget and the verify alternation. It reports handled when
// the reconcile ends here, which is either of the two outcomes that are not progress:
// the step is still working, so requeue and wait, or it died, so the goal settles as
// stalled naming the step's own error. A goal with nothing in flight passes through.
func (g *Reconciler) observeInFlight(ctx context.Context, r resource.Resource, status *Status, specHash string) (observation, reconcile.Result, bool, error) {
	if status.InFlight == nil {
		return observation{}, reconcile.Result{}, false, nil
	}
	obs := observation{kind: status.InFlight.Kind}
	job, err := g.jobs.Get(ctx, status.InFlight.JobID)
	switch {
	case err != nil:
		// The job record is gone; treat the step as lost and retry from clean.
		status.InFlight = nil
	case job.State == jobs.StateRunning || job.State == jobs.StatePending:
		return obs, reconcile.Result{RequeueAfter: g.poll}, true, nil // still working
	case job.State == jobs.StateDead:
		status.InFlight = nil
		status.Phase = PhaseStalled
		status.Message = "step failed: " + job.LastError
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "StepFailed", Message: job.LastError}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "StepFailed"}, g.clk.Now())
		res, err := g.terminal(ctx, r, *status, specHash)
		return obs, res, true, err
	default: // StateDone: a step completed.
		status.InFlight = nil
		obs.completed = true
		// Alternate building and verifying. A completed build step leaves the
		// current item's declared check unrun, so the next dispatch is that
		// check; observing the check clears the mark and the next dispatch
		// builds again. Keeping this on the status rather than in memory is what
		// makes the alternation survive a crash: a run that restarted here would
		// otherwise build twice and verify once, and the ledger would lag the
		// work by a step.
		if g.ledgerGated() {
			status.markVerifyPending(obs.kind)
		}
		// A step that parked the goal (ErrWaiting) made no progress, so it
		// does not count against the step budget: a fan-out whose children
		// outlast the budget's worth of re-checks must wait, not false-stall.
		// A planning step is not building either: it is the phase that decides
		// what the build budget will be spent on, so charging the budget for it
		// would make a goal that plans strictly poorer than one that does not.
		// Nor is a verification: it is the run checking work already paid for,
		// and charging it would halve the build budget of every goal that
		// proves its items against one that merely claims them.
		if status.WaitingSince == nil && chargesBuildBudget(obs.kind) {
			status.Steps++
			// Fold the just-completed build step into the idle streak, when a probe is
			// wired. Only a build step counts: a planning step and a parked wait are not
			// the agent failing to get anywhere. The stall itself is deferred to the
			// no-progress guard below, so a step that both finished the work and changed
			// nothing converges for that reason first.
			if g.progress != nil {
				if err := g.observeProgress(ctx, r, status); err != nil {
					return obs, reconcile.Result{}, true, err
				}
			}
		}
	}
	return obs, reconcile.Result{}, false, nil
}

// planGate holds a planning goal at its planning phase and reports whether it handled
// the reconcile. A goal that plans expands its objective into a ledger before it
// builds anything, so the first dispatch is a planning step and the stop condition is
// not evaluated until there is a record to evaluate it against. A planner that ran and
// produced nothing leaves a goal with no definition of done, which is a stall: letting
// it build anyway is how a run ends up claiming success against a record that never
// said what success was.
func (g *Reconciler) planGate(ctx context.Context, r resource.Resource, spec Spec, status Status, specHash string) (reconcile.Result, bool, error) {
	switch {
	case !g.planning:
		return reconcile.Result{}, false, nil
	case !status.Planned:
		res, err := g.dispatch(ctx, r, status, specHash, PlanJobKind, PhasePlanning, "PlanDispatched")
		return res, true, err
	case len(spec.Ledger) == 0:
		status.Phase = PhaseStalled
		status.Message = "planning produced an empty ledger"
		status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "EmptyLedger", Message: status.Message}, g.clk.Now())
		status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
		res, err := g.terminal(ctx, r, status, specHash)
		return res, true, err
	}
	return reconcile.Result{}, false, nil
}

// admit checks the desired-state records a reconcile reads (the ledger, the unit graph
// and the run's terms) and brings the status's observation of each into line with it.
// Everything it
// refuses is a terminal spec fault, because all of it means the same thing: the
// definition the run is being judged against changed underneath the run.
//
// A ledger that lost or rewrote an item is the definition of done being edited
// mid-run. A unit graph with a cycle, an edge to a unit that does not exist, or a unit
// with no way to prove it is a spec that could never run, and it is refused whole
// before anything is dispatched, because discovering it halfway through a fan-out
// means children are already running against it. A unit altered or dropped after its
// child was created is the meaning of work already in flight being rewritten; units
// nothing has been spent on are not a commitment and may still be added, removed and
// reordered.
//
// An invariant dropped or reworded after the run adopted it is the terms of the run
// being renegotiated by the run, which is the move invariants exist to foreclose, so it
// is refused here whether or not an auditor is wired to check those terms. Adding a term
// is always allowed: the rule is one-directional because tightening the terms mid-run is
// its author's to do and loosening them is nobody's.
//
// A goal that carries none of these records passes straight through, so this changes
// nothing for a goal that neither plans, fans out, nor states any terms.
func admit(spec Spec, status *Status) error {
	if err := status.ValidateLedger(spec.Ledger); err != nil {
		return fault.Wrap(fault.Terminal, "goal_ledger_regressed", err)
	}
	status.SyncLedger(spec.Ledger)
	if err := ValidateUnits(spec.Units); err != nil {
		return fault.Wrap(fault.Terminal, "goal_unit_graph_invalid", err)
	}
	if err := status.ValidateDispatched(spec.Units); err != nil {
		return fault.Wrap(fault.Terminal, "goal_unit_rewritten", err)
	}
	status.SyncUnits(spec.Units)
	if err := ValidateInvariants(spec.Invariants); err != nil {
		return fault.Wrap(fault.Terminal, "goal_invariants_invalid", err)
	}
	if err := status.ValidateInvariantsAdopted(spec.Invariants); err != nil {
		return fault.Wrap(fault.Terminal, "goal_invariant_relaxed", err)
	}
	status.SyncInvariants(spec.Invariants)
	return nil
}

// stopGuard reports the reason a goal that has not converged must stop anyway, with the
// message that reason carries, or "" to keep going. A guard that cannot answer returns an
// error for the caller to classify rather than reading as a stall.
//
// The order is the ranking: the reasons run from the most specific account of the halt to
// the least, and the first to fire is the one the run settles under. Budget and spend come
// first because a run that has used up what it was given has stopped for that reason
// whatever else was also true. No progress comes next, since a run that did nothing at all
// is better described that way than by what it was told. Non-convergence is last: it is
// what remains when a run was busy, was within its allowance, and still got nowhere, and it
// is the only one of the four that would otherwise have spent the whole budget to be
// reached as a budget reason.
func (g *Reconciler) stopGuard(ctx context.Context, r resource.Resource, spec Spec, status Status) (reason, message string, err error) {
	if status.Steps >= maxSteps(spec) {
		return "BudgetExhausted", "step budget exhausted before the stop condition was met", nil
	}
	// Our own ceiling on tokens, cost and share of the plan window, checked alongside the
	// step budget because a step is the wrong unit for cost. Crossing it names what the run
	// spent against what it was allowed, distinct from the step-budget reason and from a
	// provider pause or a transient retry, which this never touches.
	if !spec.Budget.IsZero() {
		reason, message, err := g.spendGuard(ctx, r, spec)
		if err != nil || reason != "" {
			return reason, message, err
		}
	}
	if status.StalledForNoProgress() {
		return "NoProgress", status.NoProgressReason(), nil
	}
	if status.StalledForNonConvergence() {
		return "NotConverging", status.NonConvergenceReason(), nil
	}
	return "", "", nil
}

// ledgerGated reports whether this reconciler closes the ledger loop: it has both a record
// to read verifications from and a gate to judge them with. The two are set together, so
// this is one question rather than two, and every ledger-loop branch asks it in one place
// instead of re-deriving the condition.
func (g *Reconciler) ledgerGated() bool { return g.evidence != nil && g.gate != nil }

// markVerifyPending records what the just-observed job means for the build/verify
// alternation: a completed build step leaves the current item's declared check unrun, and
// observing that check clears the mark. A planning step touches neither, because planning
// decides what the work is rather than doing any of it. An empty kind is a reservation
// written before this alternation existed, which reads as the build step it was.
func (s *Status) markVerifyPending(kind string) {
	switch kind {
	case StepJobKind, "":
		s.VerifyPending = true
	case VerifyJobKind:
		s.VerifyPending = false
	}
}

// countsAsCycle reports whether this reconcile is standing at the end of one complete
// attempt at the work, which is the unit non-convergence counts in. Getting this unit
// right is what keeps the count meaningful: too coarse and a run that is genuinely stuck
// takes several cycles to be caught, too fine and every re-check tick that changes nothing
// reads as another identical refusal and a healthy goal is stopped in seconds.
//
// A pass that observed no completed job is not a cycle, so a re-check poll and the very
// first pass of a goal that has not run anything yet are both ignored. A planning step is
// not a cycle either: planning decides what the work is, and the refusal standing before
// any of it has been attempted says nothing about whether attempting it will help.
//
// Under the ledger gate a cycle is a build step and then that item's declared check, so
// the boundary is the pass that observed the check rather than the pass that observed the
// build: VerifyPending is set by the build and cleared by the check, so waiting for it to
// be clear lands on the pass where the item's own feedback is current. Without the gate
// nothing sets it and every observed build step is its own cycle, which is the correct
// unit for a goal that has no per-item check to wait for.
func (g *Reconciler) countsAsCycle(observed bool, kind string, status Status) bool {
	return observed && kind != PlanJobKind && !status.VerifyPending
}

// chargesBuildBudget reports whether a completed job of this kind counts against the
// goal's step budget. Only building does. Planning is the phase that decides what the
// budget will be spent on, so charging it would make a goal that plans strictly poorer
// than one that does not; a verification is the run checking work already paid for, so
// charging it would halve the build budget of every goal that proves its items against one
// that merely claims them.
func chargesBuildBudget(kind string) bool {
	return kind != PlanJobKind && kind != VerifyJobKind
}

// nextJobKind chooses what the goal dispatches next. Under the ledger loop a build step is
// followed by the current item's declared check, so exactly one verification runs per build
// step rather than one per reconcile tick; with nothing left to check, or with the loop
// open, it builds.
func (g *Reconciler) nextJobKind(spec Spec, status *Status) (kind, reason string) {
	if g.ledgerGated() && status.VerifyPending {
		if _, ok := status.CurrentItem(spec.Ledger); ok {
			return VerifyJobKind, "VerifyDispatched"
		}
		status.VerifyPending = false // nothing left to check; build again
	}
	return StepJobKind, "StepDispatched"
}

// settleLedger folds the run's own record back into its ledger: every unproven item the
// evidence gate admits flips to proven, consuming the verification that proved it. It
// returns the verifications it read, so a completion refused moments later can name why
// each remaining item is unproven without reading the record twice.
//
// This is the only path to a proven item on the run path. It reads the durable record
// rather than trusting a claim, which is what makes the per-item state a projection of the
// spine instead of a second opinion about it.
func (g *Reconciler) settleLedger(ctx context.Context, r resource.Resource, status *Status) ([]Verification, error) {
	if !g.ledgerGated() || !status.Planned || len(status.Unproven()) == 0 {
		return nil, nil
	}
	recorded, err := g.evidence.Recorded(ctx, r)
	if err != nil {
		return nil, err
	}
	if status.ProveRecorded(g.gate, recorded, g.clk.Now()) > 0 {
		// The feedback describes a check that failed on an item now proven or moved
		// past, so it must not ride into the next step.
		status.ItemFeedback = ""
	}
	return recorded, nil
}

// holdsClaimAgainstLedger reports whether this goal's completion claim has to answer to
// its ledger. An unplanned goal, or one whose ledger is empty, does not: LedgerSettled is
// false for an empty ledger, so without this such a goal could never converge at all.
func (g *Reconciler) holdsClaimAgainstLedger(spec Spec, status Status) bool {
	return g.ledgerGated() && g.ledgerConverge &&
		status.Planned && len(spec.Ledger) > 0 && !status.LedgerSettled()
}

// refuseCompletion settles a goal whose model reported success over an unproven ledger,
// naming each unproven item and the reason the gate would refuse it.
//
// This is the line that must not be softened. An "unless" here (a grace pass, an
// override, a claim trusted because its check was awkward to run) restores exactly the
// prose completion the ledger replaced.
func (g *Reconciler) refuseCompletion(status *Status, recorded []Verification) {
	reasons := status.UnprovenReasons(g.gate, recorded)
	status.Phase = PhaseStalled
	status.Message = fmt.Sprintf("completion reported with %d planned item(s) unproven: %s",
		len(reasons), strings.Join(reasons, "; "))
	status.SetCondition(Condition{Type: CondStalled, Status: "True", Reason: "LedgerUnproven", Message: status.Message}, g.clk.Now())
	status.SetCondition(Condition{Type: CondReconciling, Status: "False", Reason: "Stalled"}, g.clk.Now())
}

// observeProgress folds the just-completed build step into the idle streak from the
// probe's fingerprint of the durable record, and stamps the stalling warning for the
// next step onto the status. It does not stall the goal itself: that is the no-progress
// guard's job, run after the stop condition so a step that changed nothing but finished
// the work still converges. A probe error is returned for the reconciler to classify —
// a transient read failure retries rather than being read as a stall.
func (g *Reconciler) observeProgress(ctx context.Context, r resource.Resource, status *Status) error {
	fingerprint, summary, err := g.progress.Progress(ctx, r)
	if err != nil {
		return err
	}
	streak := status.ObserveProgress(fingerprint, summary)
	status.ProgressNudge = ProgressWarning(streak)
	return nil
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
		// The ledger has two writers: the worker appends items when it plans, and the
		// reconciler marks them proven when the record backs them. So it is merged
		// rather than taken from either side. Carrying the worker's copy wholesale
		// would drop a proof this very pass admitted; carrying ours would drop an item
		// a concurrent planning step just appended.
		var proved bool
		status.Ledger, proved = mergeLedger(status.Ledger, cur.Ledger)
		// The failing check's detail is the worker's, and a verify job lands in this
		// same window: it is enqueued before this reservation is written, so a worker
		// that claims and finishes it first would otherwise have it erased here, and the
		// next build step would be asked to fix an item without being told what its own
		// check reported. The exception is a pass that just proved something, which is
		// the pass that cleared the feedback on purpose.
		if !proved {
			status.ItemFeedback = cur.ItemFeedback
		}
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

// mergeLedger reconciles this reconcile's per-item state with the copy on the freshest
// record, and reports whether the merge carried a proof the record did not already have.
//
// The shape of the ledger comes from theirs: it is the copy written against the newest
// version of the goal, so a planning step that appended items in this window is respected
// and this write does not shorten the plan. The proven marks come from whichever side has
// them, because a proof only ever goes from unset to set and never back. An item that is
// proven on either copy is proven, and the earlier proof's evidence and timestamp are kept
// intact rather than restamped.
func mergeLedger(mine, theirs []LedgerState) ([]LedgerState, bool) {
	proofs := make(map[string]LedgerState, len(mine))
	for _, st := range mine {
		if st.Proven {
			proofs[st.ID] = st
		}
	}
	added := false
	out := make([]LedgerState, len(theirs))
	copy(out, theirs)
	for i, st := range out {
		if st.Proven {
			continue
		}
		if p, ok := proofs[st.ID]; ok {
			out[i] = p
			added = true
		}
	}
	return out, added
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
