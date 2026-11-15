package goal

import (
	"context"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

type observation struct {
	completed bool
	kind      string
}

// observeInFlight reports handled when the step is still running (requeue) or died (stall
// with its error). A lost job retries from clean.
func (g *Reconciler) observeInFlight(ctx context.Context, r resource.Resource, status *Status, specHash string) (observation, reconcile.Result, bool, error) {
	if status.InFlight == nil {
		return observation{}, reconcile.Result{}, false, nil
	}
	obs := observation{kind: status.InFlight.Kind}
	job, err := g.jobs.Get(ctx, status.InFlight.JobID)
	switch {
	case err != nil:
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
		// The build/verify mark is on the status so the alternation survives a crash.
		if g.ledgerGated() {
			status.markVerifyPending(obs.kind)
		}
		// A parking step is not charged, so a fan-out whose children outlast the budget
		// waits instead of stalling.
		if status.WaitingSince == nil && chargesBuildBudget(obs.kind) {
			status.Steps++
			// The stall comes later, so a step that finished the work converges first.
			if g.progress != nil {
				if err := g.observeProgress(ctx, r, status); err != nil {
					return obs, reconcile.Result{}, true, err
				}
			}
		}
	}
	return obs, reconcile.Result{}, false, nil
}

// planGate dispatches planning before the stop condition is evaluated. An empty ledger
// stalls a run that requires proof, because there is nothing to prove. Other runs build,
// because some objectives have nothing to check and a forced check invents a file to find.
func (g *Reconciler) planGate(ctx context.Context, r resource.Resource, spec Spec, status Status, specHash string) (reconcile.Result, bool, error) {
	switch {
	case !g.planning:
		return reconcile.Result{}, false, nil
	case !status.Planned:
		res, err := g.dispatch(ctx, r, status, specHash, PlanJobKind, PhasePlanning, "PlanDispatched")
		return res, true, err
	case len(spec.Ledger) == 0 && g.ledgerConverge:
		status.stall("EmptyLedger", "planning produced an empty ledger", g.clk.Now())
		res, err := g.terminal(ctx, r, status, specHash)
		return res, true, err
	}
	return reconcile.Result{}, false, nil
}

// markVerifyPending treats an empty kind as a build, because older reservations carry none.
func (s *Status) markVerifyPending(kind string) {
	switch kind {
	case StepJobKind, "":
		s.VerifyPending = true
	case VerifyJobKind:
		s.VerifyPending = false
	}
}

// countsAsCycle defines the non-convergence unit. Counting re-check ticks would stop a
// healthy goal in seconds. Planning is excluded, because no work was tried. Under the
// ledger gate a cycle is a build then its check.
func (g *Reconciler) countsAsCycle(observed bool, kind string, status Status) bool {
	return observed && kind != PlanJobKind && !status.VerifyPending
}

// chargesBuildBudget charges only builds. Charging planning penalizes planning, and
// charging verification halves the budget of every goal that proves its items.
func chargesBuildBudget(kind string) bool {
	return kind != PlanJobKind && kind != VerifyJobKind
}

// nextJobKind runs one verification per build, never one per tick.
func (g *Reconciler) nextJobKind(spec Spec, status *Status) (kind, reason string) {
	if g.ledgerGated() && status.VerifyPending {
		if _, ok := status.CurrentItem(spec.Ledger); ok {
			return VerifyJobKind, "VerifyDispatched"
		}
		status.VerifyPending = false // nothing left to check; build again
	}
	return StepJobKind, "StepDispatched"
}

// observeProgress only counts and warns. The stall runs after the stop condition, and a
// probe error is returned so a transient failure retries.
func (g *Reconciler) observeProgress(ctx context.Context, r resource.Resource, status *Status) error {
	fingerprint, summary, err := g.progress.Progress(ctx, r)
	if err != nil {
		return err
	}
	streak := status.ObserveProgress(fingerprint, summary)
	status.ProgressNudge = ProgressWarning(streak)
	return nil
}

// dispatch is shared by every job kind, so planning gets the same lease, crash-resume and
// retry as building.
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
	// The requeue covers a lost bus signal.
	return reconcile.Result{RequeueAfter: g.poll}, nil
}
