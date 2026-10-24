package goal

// The step lifecycle: what a reconcile learns from the step it had in flight, what it
// dispatches next, and what a completed step costs the goal.

import (
	"context"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// observation is what a reconcile learned from the step it had in flight, read further
// down by the guards that apply only to a cycle in which the agent did something.
type observation struct {
	completed bool
	kind      string
}

// observeInFlight resolves the step this goal had in flight and folds its outcome into
// the status: a lost job clears the mark and retries from clean, a completed one banks
// its progress against the budget and the verify alternation. It reports handled when the
// reconcile ends here: the step is still working (requeue), or it died and the goal
// settles as stalled naming the step's own error. A goal with nothing in flight passes
// through.
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
		res, handled, err := g.stalled(ctx, r, status, specHash, "StepFailed", "step failed: "+job.LastError)
		return obs, res, handled, err
	default: // StateDone: a step completed.
		status.InFlight = nil
		obs.completed = true
		// Alternate building and verifying: a completed build leaves the current item's
		// check unrun, so the next dispatch is that check, and observing it clears the
		// mark. The mark lives on the status so the alternation survives a crash; in
		// memory, a restart would build twice and verify once.
		if g.ledgerGated() {
			status.markVerifyPending(obs.kind)
		}
		// A step that parked the goal (ErrWaiting) made no progress, so it is not charged:
		// a fan-out whose children outlast the budget's worth of re-checks must wait and
		// not false-stall. Planning and verification are not charged either (see
		// chargesBuildBudget).
		if status.WaitingSince == nil && chargesBuildBudget(obs.kind) {
			status.Steps++
			// Only a build step counts toward the idle streak; a planning step and a
			// parked wait are not the agent failing to get anywhere. The stall itself is
			// the no-progress guard's, below, so a step that finished the work and changed
			// nothing converges first.
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
// the reconcile. The first dispatch is a planning step, and the stop condition is not
// evaluated until there is a ledger to evaluate it against. A planner that produced
// nothing leaves a goal with no definition of done, which stalls: building anyway is how
// a run claims success against a record that never said what success was.
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
// alternation: a completed build leaves the current item's check unrun, and observing
// that check clears the mark. A planning step touches neither. An empty kind is a
// reservation written before the alternation existed, and reads as the build it was.
func (s *Status) markVerifyPending(kind string) {
	switch kind {
	case StepJobKind, "":
		s.VerifyPending = true
	case VerifyJobKind:
		s.VerifyPending = false
	}
}

// countsAsCycle reports whether this reconcile ends one complete attempt at the work, the
// unit non-convergence counts in. Too coarse and a stuck run takes several cycles to be
// caught; too fine and every re-check tick that changes nothing reads as another
// identical refusal and stops a healthy goal in seconds.
//
// A pass that observed no completed job is not a cycle, and neither is a planning step:
// a refusal standing before any work was attempted says nothing about whether attempting
// it will help. Under the ledger gate a cycle is a build and then that item's check, so
// the boundary is the pass where VerifyPending clears and the item's feedback is current.
// Without the gate nothing sets it and every observed build is its own cycle.
func (g *Reconciler) countsAsCycle(observed bool, kind string, status Status) bool {
	return observed && kind != PlanJobKind && !status.VerifyPending
}

// chargesBuildBudget reports whether a completed job of this kind counts against the
// goal's step budget. Only building does. Planning decides what the budget is spent on,
// so charging it would make a goal that plans poorer than one that does not; a
// verification checks work already paid for, so charging it would halve the build budget
// of every goal that proves its items.
func chargesBuildBudget(kind string) bool {
	return kind != PlanJobKind && kind != VerifyJobKind
}

// nextJobKind chooses what the goal dispatches next. Under the ledger loop a build step is
// followed by the current item's check, so one verification runs per build step and not
// one per reconcile tick; with nothing left to check, or the loop open, it builds.
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
// next step onto the status. Stalling is the no-progress guard's job, run after the stop
// condition so a step that changed nothing but finished the work still converges. A probe
// error is returned for the reconciler to classify, so a transient read failure retries
// and is not read as a stall.
func (g *Reconciler) observeProgress(ctx context.Context, r resource.Resource, status *Status) error {
	fingerprint, summary, err := g.progress.Progress(ctx, r)
	if err != nil {
		return err
	}
	streak := status.ObserveProgress(fingerprint, summary)
	status.ProgressNudge = ProgressWarning(streak)
	return nil
}

// dispatch enqueues one job of the given kind against the goal and records it in flight
// under the phase that job puts the goal in. Planning and building share it, so a
// planning step gets the same reservation, lease, crash-resume and retry behaviour a
// build step has.
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
	// The worker's bus signal makes observation prompt; the requeue makes it certain if
	// that signal is lost.
	return reconcile.Result{RequeueAfter: g.poll}, nil
}
