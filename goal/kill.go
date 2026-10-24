package goal

import (
	"context"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// ErrKillWithdrawn reports a kill removed from the spec after the reconciler stopped the
// run under it.
var ErrKillWithdrawn = errors.New("goal: the kill was withdrawn after the run was stopped by it")

// KilledReason is the stall reason a goal settles under when an operator killed it. It
// is its own reason because a run that ran out of budget, a run that stopped getting
// anywhere and a run a person stopped are three outcomes, and only the last has somebody
// to ask about it.
const KilledReason = "Killed"

// Kill is an operator's order to stop a run, carried on the goal's desired state so it
// reaches a run this process is not driving. A steer says keep going differently; a kill
// says stop.
//
// A kill is the order; a halt ([Halter]) is the enforcement at the dispatch waist, the
// one a safety breaker trips on its own. Keeping them apart lets the record say who
// stopped the run. The reconciler engages the halt the moment it reads the order, so the
// step in flight ends at its next model or tool call; a command already running in the
// sandbox runs to its end, bounded by its resource limits.
//
// A kill cannot be taken back: removing it after the run stopped under it is a terminal
// fault ([ErrKillWithdrawn]), so the record cannot read as a run that stopped on its own.
// Restarting the work is a new goal. Reason is prose that nothing branches on.
type Kill struct {
	Reason string `json:"reason,omitempty"`
}

// Halter is the run-level halt an operator's kill engages: the enforcement state at the
// dispatch waist that refuses every action a halted run attempts. The reconciler holds
// one so a kill takes effect inside the step that is running.
//
// It has two methods because a halt nobody can prove is worse than none. A composition
// that wires a halter asks it, in-process and against its own enforcement path, to show
// that a halted run is refused, and refuses to assemble if it cannot: the standard
// [NewEvidenceGate] is held to, for the same reason told there.
//
// Engaging is idempotent and keeps the first reason, so a reconcile that reads the same
// order twice does not overwrite what stopped the run.
type Halter interface {
	// Engage halts run, recording reason. The reason reaches the refusal the run's next
	// action is given, so it is what the person handed the stopped run reads.
	Engage(run, reason string)
	// ProveHalts demonstrates against this halter's own enforcement path that a run it has
	// halted is refused and a run it has not is not, and returns an error naming what it
	// caught if either is untrue. It runs at composition time, synchronously and in-process,
	// and touches no run that exists, so it takes no context.
	ProveHalts() error
}

// WithHalt gives the reconciler the halt an operator's kill engages, so a kill reaches
// the waist inside the running step and not at its boundary.
//
// Without one a kill still stops the goal: it settles as killed and no further step is
// dispatched. What is lost is promptness. A host that wires no halter gets that weaker
// behaviour and not a stall, because unlike a missing judge there is nothing here the
// goal cannot decide for itself.
func WithHalt(h Halter) Option { return func(g *Reconciler) { g.halt = h } }

// ValidateKillGiven refuses a kill removed from the spec after the run was stopped under
// it. A goal nobody killed, and a kill still on the spec, both pass.
func (s Status) ValidateKillGiven(k *Kill) error {
	if s.Killed == nil || k != nil {
		return nil
	}
	return ErrKillWithdrawn
}

// KillMessage renders an operator's kill as the message the stopped goal carries.
func KillMessage(k Kill) string {
	msg := "stopped by the operator"
	if reason := strings.TrimSpace(k.Reason); reason != "" {
		msg += ": " + reason
	}
	return msg
}

// applyKill stops a goal its operator has killed, and reports whether it handled the
// reconcile. It runs directly after admission and above everything else, including
// observing the step in flight, because a kill outranks the state of the work: a run
// mid-step, parked on its children or one verification from converging is stopped the
// same way.
//
// The halt is engaged before the record is written. The write makes the stop durable and
// the engage makes it prompt, so a crash in between leaves a run halted in memory over a
// record that has not moved, and the next pass reads the order again and settles it. The
// other order would leave a settled record over a run still dispatching actions.
func (g *Reconciler) applyKill(ctx context.Context, r resource.Resource, spec Spec, status *Status, specHash string) (reconcile.Result, bool, error) {
	if spec.Kill == nil {
		return reconcile.Result{}, false, nil
	}
	msg := KillMessage(*spec.Kill)
	if g.halt != nil {
		g.halt.Engage(r.Name, msg)
	}
	if status.Killed == nil {
		now := g.clk.Now()
		status.Killed = &now
	}
	// The step in flight is abandoned and not awaited: its actions are refused from
	// here, and a goal still recording it as in flight would read as working.
	status.InFlight = nil
	status.WaitingSince = nil
	return g.stalled(ctx, r, status, specHash, KilledReason, msg)
}
