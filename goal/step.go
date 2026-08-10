package goal

// The step lifecycle: what a reconcile learns from the step it had in flight, what
// it dispatches next, and what a completed step costs the goal. The build/verify
// alternation and the step budget both live here.

import (
	"context"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

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
		status.stall("StepFailed", "step failed: "+job.LastError, g.clk.Now())
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
		status.stall("EmptyLedger", "planning produced an empty ledger", g.clk.Now())
		res, err := g.terminal(ctx, r, status, specHash)
		return res, true, err
	}
	return reconcile.Result{}, false, nil
}

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
