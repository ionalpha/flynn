package goal

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// Running a unit graph is the reconciler's other loop: while a goal's graph is
// unsettled the goal does not build, it admits. Every unit whose dependencies are
// proven is created as a governed child, the parent parks on those children, and
// each unit settles from what its child recorded. Only once the graph is settled
// does the goal go back to being an ordinary goal: it builds, it is judged against
// its ledger, and its stop condition is evaluated.
//
// That ordering is the convergence gate, and it is structural rather than a check
// someone remembered to write. A goal with an unsettled graph returns before the stop
// evaluator is ever consulted, so there is no path on which a model's opinion that
// the work is done can outrank a graph with unproven units in it.

// UnitSpawner creates the child goal that runs one unit of a goal's graph, and
// reports what those children produced. It is the reconciler's port onto fan-out,
// declared here rather than taken from the fan-out package because the concrete
// spawner is built on top of this package and cannot be imported back into it.
//
// A nil spawner leaves plan-driven fan-out off. It does not leave it silently off: a
// goal that carries a graph with no spawner wired stalls, because a graph that is
// admitted, validated and then never run is exactly the loaded-and-does-nothing
// failure the rest of this package refuses to ship.
type UnitSpawner interface {
	// Spawn creates the child goal that runs u on behalf of parent and returns its
	// id. It is governed at the same waist a model-driven spawn is: same grant
	// intersection, same metering, same record. Plan-driven fan-out is only worth
	// having if it is not a way around the rules the other path obeys.
	//
	// Spawn must be idempotent for a given parent and unit: called twice it returns
	// the same child, and creates and charges for only one. The reconciler records
	// the child it was handed after the call returns, so a crash in that window
	// re-enters Spawn on resume, and an implementation that creates a second child
	// there turns every crash mid-fan-out into duplicated work charged twice to a
	// shared budget. Deriving the child's name from the parent and the unit id is
	// enough to satisfy this.
	//
	// A refusal that would succeed if it were tried again (a concurrency cap, a
	// contended write) must be classified Transient: the reconciler retries those and
	// settles the unit as failed under every other class, so a temporary refusal
	// classified as anything else permanently kills a unit that was merely early.
	Spawn(ctx context.Context, parent resource.Resource, u Unit) (childID string, err error)
	// Outcomes reports what the given children have produced. A child still running is
	// reported with Done false, or omitted; either reads as still running.
	Outcomes(ctx context.Context, childIDs []string) ([]UnitOutcome, error)
}

// UnitOutcome is what one child produced, as the parent needs to read it.
//
// Evidence is the whole judgement. A finished child unblocks the units downstream of
// it when, and only when, it carries a reference to the verification that proves its
// own unit; a child that finished without one settles its unit as failed however
// confidently it reported success. That is deliberately strict, and it is the same
// rule the evidence gate holds one level down: a dependency edge is waiting on proof,
// and if "the child exited" were enough, dependsOn would quietly come to mean that
// instead.
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

// WithUnitSpawner runs a goal's unit graph: ready units are created as governed
// children through s, the parent parks on them, and each unit settles from what its
// child recorded. Without it a goal that carries a graph stalls rather than quietly
// ignoring it.
func WithUnitSpawner(s UnitSpawner) Option { return func(g *Reconciler) { g.units = s } }

// unitFanoutReason is the condition reason a parent carries while its children run.
const unitFanoutReason = "AwaitingUnits"

// noVerificationFailure is what a unit's record says when its child finished and
// produced no proof, which is a different thing from a child that failed and said
// why. Naming it keeps the stall legible instead of trailing off after the unit id.
const noVerificationFailure = "the child finished without recording a verification"

// advanceUnits runs one pass of the goal's unit graph and reports whether it handled
// the reconcile. It handles the pass whenever the graph is unsettled, which is what
// keeps the goal from building, being judged, or converging while units are still
// outstanding; once the graph settles it hands back and the goal carries on as an
// ordinary goal.
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
	// Nothing running and nothing ready over an unsettled graph: a unit has failed and
	// what it was holding up can never be reached. That is a distinct outcome from a
	// slow fan-out, and without settling it here the parent would poll children that no
	// longer exist until something else stopped it.
	_, reason := status.UnitStalled(spec.Units)
	return g.stallUnits(ctx, r, status, specHash, "UnitGraphStalled", reason)
}

// settleUnits folds each finished child back into its unit. A child carrying
// evidence proves its unit; one that finished without any settles it failed, which
// leaves every unit downstream of it blocked rather than letting an unproven result
// travel down a dependency edge.
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
			// The outcome names a unit the record cannot settle, so the record and the
			// spawner disagree about what this goal has running. That is not a state to
			// carry on from.
			return fault.Wrap(fault.Terminal, "goal_unit_settle", serr)
		}
	}
	return nil
}

// admitReadyUnits creates a child for every unit whose dependencies are proven, and
// records the children it was handed.
//
// The children are created before the record is written, which is the safe order
// only because Spawn is required to be idempotent per unit: a crash between the
// create and the write re-enters Spawn on resume and is handed the same child back,
// so the fan-out resumes rather than doubling. The opposite order (reserve, then
// create) trades that for a reservation whose child may or may not exist, which
// cannot be told apart from a spawn that transiently failed and should be retried.
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
			// Nothing was recorded for this unit, so the retry starts from where this
			// pass began. Units already admitted in this pass are written below on the
			// way out, so a transient refusal costs a retry rather than their record.
			if perr := g.persistUnits(ctx, r, *status, specHash); perr != nil {
				return perr
			}
			return err
		}
		// A refusal that will not come good (past the delegation depth, outside the
		// grant, out of budget) is this unit's outcome, not the goal's error. Recording
		// it settles the unit unproven, so the graph stalls naming the refusal instead
		// of retrying a spawn that has already been decided.
		if merr := status.RefuseUnit(u.ID, err.Error(), now); merr != nil {
			return fault.Wrap(fault.Terminal, "goal_unit_refuse", merr)
		}
	}
	return g.persistUnits(ctx, r, *status, specHash)
}

// parkOnUnits stands the parent down while its children run. It reuses the park the
// model-driven fan-out already uses: no step is dispatched, no budget is spent, and a
// settling child clears the mark and signals the parent (see wakeOwner), with the
// recheck fallback covering a lost wake. That is what keeps a wait costed by child
// state-changes rather than by a durable step per poll.
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

// stallUnits settles the goal because its graph cannot proceed, naming what stranded
// it.
func (g *Reconciler) stallUnits(ctx context.Context, r resource.Resource, status *Status, specHash, reason, message string) (reconcile.Result, bool, error) {
	status.WaitingSince = nil
	status.stall(reason, message, g.clk.Now())
	// persistUnits rather than terminal's blind write: this pass may already have
	// written the record when it admitted units, so the resource in hand is a version
	// behind and a blind Put would lose the race and drop the stall.
	if err := g.persistUnits(ctx, r, *status, specHash); err != nil {
		return reconcile.Result{}, true, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, true, nil
}

// persistUnits writes the reconciler's view of the fan-out onto a fresh read of the
// record. It applies only the fields this loop owns, so a worker that persisted a
// checkpoint or a ledger proof in the same window keeps it: the two writers overlap
// here in a way they do not elsewhere, because a parked parent's children settle on
// their own schedule rather than at a step boundary.
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
// about why it finished, which a child that simply produced nothing does.
func unitFailure(failure string) string {
	if failure == "" {
		return noVerificationFailure
	}
	return failure
}
