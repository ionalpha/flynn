package goal

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// UnitSpawner creates the child goal that runs one unit of a goal's graph, and reports
// what those children produced. It is declared here and not taken from the fan-out
// package because the concrete spawner is built on this package and cannot be imported
// back into it.
//
// A nil spawner leaves plan-driven fan-out off, but not silently: a goal that carries a
// graph with no spawner wired stalls, because a graph admitted and validated but never
// run is the loaded-and-does-nothing failure this package refuses to ship.
type UnitSpawner interface {
	// Spawn creates the child goal that runs u on behalf of parent and returns its id. It
	// is governed at the same waist as a model-driven spawn (same grant intersection,
	// metering and record), so plan-driven fan-out is no way around the rules the other
	// path obeys.
	//
	// Spawn must be idempotent for a given parent and unit: called twice it returns the
	// same child and creates and charges for only one. The reconciler records the child
	// after the call returns, so a crash in that window re-enters Spawn on resume, and a
	// second child would duplicate work charged twice to a shared budget. Deriving the
	// child's name from the parent and the unit id is enough.
	//
	// A refusal that would succeed on retry (a concurrency cap, a contended write) must
	// be classified Transient: every other class settles the unit as failed, which
	// would kill a merely early unit permanently.
	Spawn(ctx context.Context, parent resource.Resource, u Unit) (childID string, err error)
	// Outcomes reports what the given children have produced. A child still running is
	// reported with Done false, or omitted; either reads as still running.
	Outcomes(ctx context.Context, childIDs []string) ([]UnitOutcome, error)
}

// UnitOutcome is what one child produced, as the parent reads it.
//
// Evidence is the whole judgement. A finished child unblocks the units downstream of it
// only when it carries a reference to the verification that proves its own unit; one that
// finished without it settles its unit as failed however confidently it reported
// success. That is the evidence gate's rule one level up: a dependency edge waits on
// proof, and if "the child exited" were enough, dependsOn would come to mean that.
type UnitOutcome struct {
	ChildID string
	// Done reports that the child reached a terminal state, whatever that state was.
	Done bool
	// Evidence is the reference to the verification that proved this child's unit.
	// Empty settles the unit unproven.
	Evidence string
	// Failure is why the child finished without proving its unit, for the record and
	// for the stall message. Ignored when Evidence is set.
	Failure string
}

// WithUnitSpawner runs a goal's unit graph: ready units are created as governed children
// through s, the parent parks on them, and each unit settles from what its child
// recorded. Without it a goal that carries a graph stalls.
func WithUnitSpawner(s UnitSpawner) Option { return func(g *Reconciler) { g.units = s } }

// unitFanoutReason is the condition reason a parent carries while its children run.
const unitFanoutReason = "AwaitingUnits"

// noVerificationFailure is what a unit's record says when its child finished and
// produced no proof, as distinct from a child that failed and said why.
const noVerificationFailure = "the child finished without recording a verification"

// advanceUnits runs one pass of the goal's unit graph and reports whether it handled the
// reconcile. While the graph is unsettled the goal does not build: every unit whose
// dependencies are proven is created as a governed child, the parent parks on them, and
// each unit settles from what its child recorded. Only once the graph settles does the
// goal go back to being an ordinary goal that builds and is judged.
//
// That ordering is the convergence gate, and it is structural: a goal with an unsettled
// graph returns before the stop evaluator is consulted, so no model opinion that the
// work is done can outrank a graph with unproven units in it.
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
		return reconcile.Result{}, false, nil // the graph is done; the goal is a goal again
	}
	if len(status.DispatchedUnits()) > 0 {
		return g.parkOnUnits(ctx, r, status, specHash)
	}
	// Nothing running and nothing ready over an unsettled graph: a unit has failed and what
	// it held up can never be reached. Settling here keeps the parent from polling
	// children that no longer exist.
	_, reason := status.UnitStalled(spec.Units)
	return g.stallUnits(ctx, r, status, specHash, "UnitGraphStalled", reason)
}

// settleUnits folds each finished child back into its unit. A child carrying evidence
// proves its unit; one without it settles its unit failed, leaving every unit downstream
// blocked.
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
			// The record and the spawner disagree about what this goal has running.
			return fault.Wrap(fault.Terminal, "goal_unit_settle", serr)
		}
	}
	return nil
}

// admitReadyUnits creates a child for every unit whose dependencies are proven, and
// records the children it was handed.
//
// Creating before recording is safe only because Spawn is idempotent per unit: a crash
// between the create and the write re-enters Spawn on resume and gets the same child
// back. Reserving first would leave a reservation whose child may or may not exist,
// indistinguishable from a spawn that transiently failed.
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
			// Nothing was recorded for this unit. Units admitted earlier in this pass are
			// written on the way out, so a transient refusal costs a retry and not their
			// record.
			if perr := g.persistUnits(ctx, r, *status, specHash); perr != nil {
				return perr
			}
			return err
		}
		// A refusal that will not come good (past the delegation depth, outside the grant,
		// out of budget) is this unit's outcome and not the goal's error: the graph stalls
		// naming it and does not retry a spawn already decided.
		if merr := status.RefuseUnit(u.ID, err.Error(), now); merr != nil {
			return fault.Wrap(fault.Terminal, "goal_unit_refuse", merr)
		}
	}
	return g.persistUnits(ctx, r, *status, specHash)
}

// parkOnUnits stands the parent down while its children run, with the park the
// model-driven fan-out uses: no step is dispatched and no budget is spent. A settling
// child clears the mark and signals the parent (see wakeOwner), and the recheck covers a
// lost wake, so a wait costs child state-changes and not a durable step per poll.
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

// stallUnits settles the goal because its graph cannot proceed, naming what stranded it.
func (g *Reconciler) stallUnits(ctx context.Context, r resource.Resource, status *Status, specHash, reason, message string) (reconcile.Result, bool, error) {
	status.WaitingSince = nil
	status.stall(reason, message, g.clk.Now())
	// Not terminal's blind write: this pass may already have written the record, leaving
	// the resource in hand a version behind, and a blind Put would lose the race and drop
	// the stall.
	if err := g.persistUnits(ctx, r, *status, specHash); err != nil {
		return reconcile.Result{}, true, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, true, nil
}

// persistUnits writes the reconciler's view of the fan-out onto a fresh read of the
// record, applying only the fields this loop owns. A worker that persisted a checkpoint
// or a ledger proof in the same window keeps it, which matters here because a parked
// parent's children settle on their own schedule and not at a step boundary.
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

// unitFailure keeps the record of a failed unit legible when its child said nothing
// about why it finished.
func unitFailure(failure string) string {
	if failure == "" {
		return noVerificationFailure
	}
	return failure
}
