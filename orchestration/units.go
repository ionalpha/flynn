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

// UnitFanout is the goal.UnitSpawner: it runs a plan's units as children of the
// [Spawner] a model's spawn uses, so depth, the concurrency cap, the shared
// budget, the narrowed grant and the owner reference apply unchanged. It is a
// separate type only because the two Spawn methods take different arguments.
type UnitFanout struct {
	sp  *Spawner
	gov Governor
}

// Governor admits, meters and records a spawn. In practice it is the
// *dispatch.Dispatcher that governs the model's own spawn tool.
type Governor interface {
	Govern(ctx context.Context, a dispatch.Action, work func(context.Context) (dispatch.Metering, error)) error
}

// Units adapts sp for goal.WithUnitSpawner; a reconciler without one stalls any
// goal that carries a unit graph. Pass the executor's own dispatcher
// (mission.Executor.Dispatcher) as gov, so both fan-out paths share one set of
// hooks. A nil gov fails the first spawn.
func Units(sp *Spawner, gov Governor) *UnitFanout { return &UnitFanout{sp: sp, gov: gov} }

var _ goal.UnitSpawner = (*UnitFanout)(nil)

// UnitGovernor builds a [Governor] when there is no single executor to borrow
// one from, as with a Router that builds a loop per (driver, model) on demand.
// It uses the model path's capability admitter and containment gate plus the
// run's brake and budget hooks, so a halt that stops the model delegating stops
// a plan too.
//
// It takes no event sink: correlation ids are per dispatcher, so two writing to
// one stream would collide. The child goal and its run record the spawn anyway.
func UnitGovernor(sb sandbox.Sandbox, hooks ...dispatch.Hook) *dispatch.Dispatcher {
	opts := []dispatch.Option{
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithHook(capability.NewContainmentGate(sb)),
	}
	for _, h := range hooks {
		if h != nil {
			opts = append(opts, dispatch.WithHook(h))
		}
	}
	return dispatch.New(opts...)
}

// Spawn creates the child for one unit, admitted under ActionSpawn against the
// parent's grant, so a goal never granted fan-out cannot get it by writing a
// plan. The grant is bound here because a reconcile carries no run's authority.
//
//   - The name derives from parent and unit, so a crash before the reconciler
//     records the child gets the same child back.
//   - The unit's verify clause is the child's ledger, so it is judged by the
//     plan's check; a child that converges unproven fails the unit.
//   - The stop condition is the unit's, or the child would run on after its
//     work is done.
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
	// An empty grant means unconstrained, as on the model path, so binding it
	// would wrongly allow nothing.
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

// Outcomes reports each child's proof of its unit, or why there is none. A
// running child is not done, which keeps the parent parked instead of settling
// on a partial run.
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

// UnitChildName is the name of the child that runs a unit. It depends only on
// its arguments, so the store answers "was this unit spawned" across restarts
// without bookkeeping a crash can lose.
func UnitChildName(parent, unitID string) string { return parent + "-unit-" + unitID }

// unitStopCondition quotes the unit's check so the child knows its one job is
// to make it pass.
func unitStopCondition(u goal.Unit) string {
	return "the unit's declared check passes: " + u.Verify
}

// unitProof returns the evidence refs that prove a finished child's unit, or a
// failure. Converged alone is a failure: it says the child's loop stopped, and
// the unit asks whether the plan's check was recorded as passing.
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

// stalledFailure keeps a stalled child's reason, or supplies one so the record
// is actionable.
func stalledFailure(message string) string {
	if strings.TrimSpace(message) == "" {
		return "the child stalled without recording why"
	}
	return message
}

// childScope returns the scope a child was created in. A child from before this
// process started reads back under the zero scope, as in Poll.
func (s *Spawner) childScope(id string) resource.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.children[id]; rec != nil {
		return rec.scope
	}
	return resource.Scope{}
}
