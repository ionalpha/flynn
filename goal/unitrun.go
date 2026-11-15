package goal

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// UnitSpawner runs each unit as a child goal. It is declared here because the concrete
// spawner imports this package. A goal with a graph and no spawner stalls, because a
// validated graph that never runs would look governed.
type UnitSpawner interface {
	// Spawn is governed like a model-driven spawn, so plan fan-out is no way around the
	// grant. It must be idempotent per parent and unit, because a crash before the child
	// is recorded re-enters it; naming the child from parent and unit id is enough. A
	// refusal that retry could clear must be Transient, because any other class fails
	// the unit for good.
	Spawn(ctx context.Context, parent resource.Resource, u Unit) (childID string, err error)
	// Outcomes may omit a running child or report it with Done false.
	Outcomes(ctx context.Context, childIDs []string) ([]UnitOutcome, error)
}

// UnitOutcome is judged on Evidence alone. A child without it fails its unit whatever it
// reported, so a dependency waits on proof and not on the child exiting.
type UnitOutcome struct {
	ChildID string
	// Done is any terminal state.
	Done     bool
	Evidence string
	// Failure is ignored when Evidence is set.
	Failure string
}

// WithUnitSpawner runs unit graphs. Without it a goal with a graph stalls.
func WithUnitSpawner(s UnitSpawner) Option { return func(g *Reconciler) { g.units = s } }

const unitFanoutReason = "AwaitingUnits"

const noVerificationFailure = "the child finished without recording a verification"

// advanceUnits returns before the stop evaluator while the graph is unsettled, so no
// model opinion outranks an unproven unit. The goal builds only after the graph settles.
func (g *Reconciler) advanceUnits(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string) (reconcile.Result, bool, error) {
	if len(spec.Units) == 0 {
		return reconcile.Result{}, false, nil
	}
	if g.units == nil {
		return g.stallUnits(ctx, r, status, specHash,
			"UnitSpawnerMissing", "the goal carries a unit graph but no spawner is wired to run it")
	}
	if err := g.settleUnits(ctx, status); err != nil {
		return reconcile.Result{}, true, err
	}
	if err := g.admitReadyUnits(ctx, r, spec, status, specHash); err != nil {
		return reconcile.Result{}, true, err
	}
	if status.UnitsSettled(spec.Units) {
		return reconcile.Result{}, false, nil
	}
	if len(status.DispatchedUnits()) > 0 {
		return g.parkOnUnits(ctx, r, status, specHash)
	}
	// Nothing running or ready means a failed unit blocks the rest for good.
	_, reason := status.UnitStalled(spec.Units)
	return g.stallUnits(ctx, r, status, specHash, "UnitGraphStalled", reason)
}

func (g *Reconciler) settleUnits(ctx context.Context, status *Status) error {
	ids := status.DispatchedUnits()
	if len(ids) == 0 {
		return nil
	}
	outcomes, err := g.units.Outcomes(ctx, ids)
	if err != nil {
		return err // classified by the spawner; a transient read retries
	}
	byChild := make(map[string]string, len(status.Units))
	for _, st := range status.Units {
		if st.ChildID != "" {
			byChild[st.ChildID] = st.ID
		}
	}
	now := g.clk.Now()
	for _, o := range outcomes {
		unitID, ours := byChild[o.ChildID]
		if !o.Done || !ours {
			continue // still running, or a child this goal did not create
		}
		var serr error
		if o.Evidence != "" {
			serr = status.MarkUnitProven(unitID, o.Evidence, now)
		} else {
			serr = status.MarkUnitFailed(unitID, unitFailure(o.Failure), now)
		}
		if serr != nil {
			// The record and the spawner disagree.
			return fault.Wrap(fault.Terminal, "goal_unit_settle", serr)
		}
	}
	return nil
}

// admitReadyUnits creates before recording, which is safe because Spawn is idempotent.
// Reserving first would leave a reservation indistinguishable from a transient failure.
func (g *Reconciler) admitReadyUnits(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string) error {
	ready := status.ReadyUnits(spec.Units)
	if len(ready) == 0 {
		return nil
	}
	now := g.clk.Now()
	for _, u := range ready {
		childID, err := g.units.Spawn(ctx, r, u)
		if err == nil {
			if merr := status.MarkUnitDispatched(u, childID); merr != nil {
				return fault.Wrap(fault.Terminal, "goal_unit_dispatch", merr)
			}
			continue
		}
		if fault.Classify(err) == fault.Transient {
			// Units admitted earlier this pass are written first, so they keep their record.
			if perr := g.persistUnits(ctx, r, *status, specHash); perr != nil {
				return perr
			}
			return err
		}
		// A permanent refusal is the unit's outcome, so the graph stalls naming it instead
		// of retrying.
		if merr := status.RefuseUnit(u.ID, err.Error(), now); merr != nil {
			return fault.Wrap(fault.Terminal, "goal_unit_refuse", merr)
		}
	}
	return g.persistUnits(ctx, r, *status, specHash)
}

// parkOnUnits spends no step or budget. A settling child wakes the parent (wakeOwner), and
// the recheck covers a lost wake.
func (g *Reconciler) parkOnUnits(ctx context.Context, r resource.Resource, status *Status, specHash string) (reconcile.Result, bool, error) {
	now := g.clk.Now()
	status.WaitingSince = &now
	status.Phase = PhaseRunning
	status.SetCondition(Condition{
		Type: CondReconciling, Status: "True", Reason: unitFanoutReason,
		Message: "waiting on the goal's unit graph",
	}, now)
	if err := g.persistUnits(ctx, r, *status, specHash); err != nil {
		return reconcile.Result{}, true, err
	}
	return reconcile.Result{RequeueAfter: g.recheckAfter()}, true, nil
}

func (g *Reconciler) stallUnits(ctx context.Context, r resource.Resource, status *Status, specHash, reason, message string) (reconcile.Result, bool, error) {
	status.WaitingSince = nil
	status.stall(reason, message, g.clk.Now())
	// Not terminal's blind Put, because this pass may have written already and the
	// resource in hand is stale.
	if err := g.persistUnits(ctx, r, *status, specHash); err != nil {
		return reconcile.Result{}, true, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, true, nil
}

// persistUnits writes only fields this loop owns onto a fresh read, because children
// settle on their own schedule and a worker write in the window must survive.
func (g *Reconciler) persistUnits(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	_, err := resource.UpdateByID(ctx, g.store, r.ID, func(fresh *resource.Resource) error {
		cur, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		cur.Units = status.Units
		cur.WaitingSince = status.WaitingSince
		cur.Phase = status.Phase
		cur.Message = status.Message
		cur.Conditions = status.Conditions
		cur.ObservedSpecHash = specHash
		enc, err := cur.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
	return putErr(err)
}

func unitFailure(failure string) string {
	if failure == "" {
		return noVerificationFailure
	}
	return failure
}
