package orchestration

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// UnitFanout is the concrete goal.UnitSpawner: it runs the units of a goal's plan as
// children of the same Spawner the model-driven path uses. It is the last piece of
// plan-driven fan-out, and it is deliberately thin, because the value of the shape is
// that a unit is spawned through the waist a model's spawn goes through rather than
// beside it. Depth, the concurrency cap, the shared budget reservation, the narrowed
// grant and the owner reference are all the Spawner's, unchanged.
//
// It is a separate type from Spawner only because Go will not let one type carry two
// methods named Spawn, and the two paths take different arguments: a model asks for a
// sub-goal, a plan names a unit.
type UnitFanout struct {
	sp  *Spawner
	gov Governor
}

// Governor is the dispatch waist a spawn is admitted, metered and recorded through. In
// a real composition it is the *dispatch.Dispatcher the executor already governs the
// model's own spawn tool with; it is named as an interface here so this states what it
// needs rather than depending on the concrete type.
type Governor interface {
	Govern(ctx context.Context, a dispatch.Action, work func(context.Context) (dispatch.Metering, error)) error
}

// Units adapts a Spawner onto the interface the goal reconciler drives its unit graph
// through, governed through gov. Wire the result with goal.WithUnitSpawner; a
// reconciler without one stalls any goal that carries a graph rather than ignoring it.
//
// Pass the executor's own dispatcher (mission.Executor.Dispatcher) rather than a fresh
// one. Handing it a second dispatcher is not wrong, but it is a second set of hooks and
// a second event sink, and then the two fan-out paths are only governed the same way by
// coincidence of configuration. A nil governor is refused at the first spawn rather than
// silently ungoverned.
func Units(sp *Spawner, gov Governor) *UnitFanout { return &UnitFanout{sp: sp, gov: gov} }

var _ goal.UnitSpawner = (*UnitFanout)(nil)

// Spawn creates the child that runs one unit, through the same dispatch waist a model's
// spawn goes through: admitted under ActionSpawn against the parent's own grant, metered,
// and recorded on the spine. A goal that was never granted the right to fan out cannot
// fan out by writing a plan instead of calling a tool, and that is the whole reason the
// grant is bound onto the context here rather than being left to whoever holds it: the
// reconciler drives this loop, and a reconcile carries no run's authority.
//
// Three further things make the child a unit's rather than a generic delegation.
//
// Its name is derived from the parent and the unit, so the spawn is idempotent: the
// reconciler records the child after the create returns, and a crash in that window
// re-enters here and is handed the same child back instead of creating a second one.
//
// It carries the unit's verify clause as a ledger of its own, so the child is judged
// against the check the plan author wrote rather than against its own account of how it
// went. That is what makes a dependency edge wait on proof: the unit is settled from
// the child's ledger, and a child that converged without proving it settles the unit
// failed.
//
// Its stop condition is the unit's, never the parent's. A child that inherits its
// parent's completion condition is the failure that leaves a fork running after the
// work it was forked for is finished.
func (f *UnitFanout) Spawn(ctx context.Context, parent resource.Resource, u goal.Unit) (string, error) {
	if f.gov == nil {
		return "", fault.New(fault.Terminal, "spawn_unit_ungoverned",
			"unit fan-out: no dispatch waist wired to admit a plan-driven spawn")
	}
	ledger, err := goal.AppendItems(nil, goal.LedgerItem{Item: u.Objective, Verify: u.Verify})
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "spawn_unit_ledger", err)
	}
	spec, err := goal.DecodeSpec(parent)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "spawn_unit_parent_decode", err)
	}
	req := Request{
		Sub:           mission.SubGoal{Objective: u.Objective, Actions: u.Actions, Agent: u.Agent},
		Name:          UnitChildName(parent.Name, u.ID),
		StopCondition: unitStopCondition(u),
		Ledger:        ledger,
	}
	// A goal with no grant is unconstrained, exactly as it is on the model path, so an
	// empty one is left unbound rather than bound as a grant that allows nothing.
	if len(spec.Grant) > 0 {
		ctx = capability.Into(ctx, capability.NewGrant(spec.Grant...))
	}
	var childID string
	err = f.gov.Govern(ctx, dispatch.Action{
		Name:  mission.ActionSpawn,
		Scope: state.Scope(parent.Scope),
		Trust: sandbox.TrustTrusted,
		Goal:  parent.Name,
	}, func(ctx context.Context) (dispatch.Metering, error) {
		id, serr := f.sp.SpawnRequest(ctx, parent, req)
		childID = id
		return dispatch.Metering{}, serr
	})
	return childID, err
}

// Outcomes reports what each child has produced, in the terms the unit graph settles
// on: a reference to the verification that proves the unit, or the reason there is
// none. A child still running is reported not done, which is what keeps the parent
// parked instead of settling its unit on a partial run.
func (f *UnitFanout) Outcomes(ctx context.Context, childIDs []string) ([]goal.UnitOutcome, error) {
	out := make([]goal.UnitOutcome, 0, len(childIDs))
	for _, id := range childIDs {
		r, err := f.sp.store.Get(ctx, goal.Kind, f.sp.childScope(id), id)
		if err != nil {
			return nil, fault.Wrap(fault.Transient, "spawn_unit_get", err)
		}
		status, err := goal.DecodeStatus(r)
		if err != nil {
			return nil, fault.Wrap(fault.Terminal, "spawn_unit_decode", err)
		}
		switch status.Phase {
		case goal.PhaseConverged, goal.PhaseStalled:
			evidence, failure := unitProof(status)
			out = append(out, goal.UnitOutcome{ChildID: id, Done: true, Evidence: evidence, Failure: failure})
			f.sp.finish(ctx, id)
		default:
			out = append(out, goal.UnitOutcome{ChildID: id})
		}
	}
	return out, nil
}

// UnitChildName is the resource name of the child that runs a unit. It is a pure
// function of the parent and the unit id, which is the whole of the idempotency the
// reconciler requires: the same unit resolves to the same child on every pass and after
// every restart, so "has this unit been spawned" is answered by the store rather than by
// bookkeeping that a crash can lose.
func UnitChildName(parent, unitID string) string { return parent + "-unit-" + unitID }

// unitStopCondition is the child's own definition of done, quoting the check the unit
// declared. The child is a run whose one job is to make that check pass, and telling it
// so in its own words is the difference between a child that knows what it is for and
// one working from a generic instruction to accomplish a sub-goal.
func unitStopCondition(u goal.Unit) string {
	return "the unit's declared check passes: " + u.Verify
}

// unitProof reads a finished child's ledger for the verification that proves its unit,
// and returns the reason there is none when there is none. Every path that does not
// produce a reference is a failure, including a child that converged: the child's phase
// says its own loop stopped, and the unit's question is narrower than that, which is
// whether the check the plan wrote was recorded as passing.
func unitProof(status goal.Status) (evidence, failure string) {
	if status.Phase == goal.PhaseStalled {
		return "", stalledFailure(status.Message)
	}
	if len(status.Ledger) == 0 {
		return "", "the child converged with no ledger to prove its unit"
	}
	refs := make([]string, 0, len(status.Ledger))
	var unproven []string
	for _, item := range status.Ledger {
		switch {
		case !item.Proven:
			unproven = append(unproven, item.ID)
		case item.Evidence == "":
			unproven = append(unproven, item.ID) // proven with nothing recorded behind it
		default:
			refs = append(refs, item.Evidence)
		}
	}
	if len(unproven) > 0 {
		return "", fmt.Sprintf("the child converged with %d of %d ledger item(s) unproven: %s",
			len(unproven), len(status.Ledger), strings.Join(unproven, ", "))
	}
	return strings.Join(refs, ","), ""
}

// stalledFailure keeps a stalled child's own account of why, and supplies one when it
// stopped without saying. A unit whose record trails off after the unit id is a stall
// nobody can act on.
func stalledFailure(message string) string {
	if strings.TrimSpace(message) == "" {
		return "the child stalled without recording why"
	}
	return message
}

// childScope resolves the scope a child was created in from the spawner's bookkeeping.
// The zero scope is the right fallback: it is what a child created before this process
// started is read back under until something adopts it, and it is what the model-driven
// Poll already uses.
func (s *Spawner) childScope(id string) resource.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.children[id]; rec != nil {
		return rec.scope
	}
	return resource.Scope{}
}
