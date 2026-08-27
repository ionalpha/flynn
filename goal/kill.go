package goal

import (
	"context"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// A kill is the operator deciding this run should stop now. It is the other half of the
// operator's surface on a run in flight: a steer says keep going and do it differently, a
// kill says stop.
//
// Two words are in play here and they name different things. A kill is the operator's
// order, recorded on the goal's desired state. A halt is the enforcement state at the
// dispatch waist, which a safety breaker trips on its own when a run goes runaway and
// which an operator's kill engages deliberately. Keeping them apart is what lets the
// record say who stopped the run and why, when the mechanism that stopped it is the same
// one a rate breaker uses.
//
// What separates a kill from a request is where it takes effect. A stop that only acts between steps
// leaves a run that has just started a long turn working for as long as that turn takes,
// which on a real step is minutes of tool calls after the operator decided it should
// stop. So the kill is not a request the run gets to finish its step before honoring: the
// reconciler engages the halt the moment it reads the order, and from there the waist
// refuses every model call and every tool call that step attempts. The step ends at its
// next action rather than at its own conclusion.
//
// What it does not do is reach into an action already running. A command in the sandbox
// when the halt engages runs to its own end, bounded by the resource limits it was
// launched under. Interrupting mid-action would need a cancellation path from the
// reconciler into whichever process holds the step's lease, and refusing at dispatch is
// what is buildable from the record alone.
//
// A kill cannot be taken back. Once the reconciler has stopped a run under one, removing
// it from the spec is refused as a terminal fault, the way a withdrawn steer and a
// reworded invariant are, and for a reason narrower than either: the record of a run
// nobody can see is all anybody has afterwards, and a run that reads as having stopped on
// its own when an operator stopped it is a record that lies about the one moment a person
// intervened. Restarting the work is a new goal, which is a different operation with a
// different name.

// ErrKillWithdrawn reports a kill removed from the spec after the reconciler stopped the
// run under it.
var ErrKillWithdrawn = errors.New("goal: the kill was withdrawn after the run was stopped by it")

// KilledReason is the stall reason a goal settles under when an operator killed it. It is
// its own reason rather than a variety of some other stop, because the question a stopped
// run is read with is which of them happened: a run that ran out of budget, a run that
// stopped getting anywhere, and a run a person stopped are three different outcomes and
// only the last one has somebody to ask about it.
const KilledReason = "Killed"

// Kill is an operator's order to stop a run, carried on the goal's desired state so it
// reaches a run this process is not driving.
//
// Reason is optional and it is the only field. What the operator was thinking is worth
// recording and nothing downstream branches on it, so it is left as prose; who issued it
// and when are not here because the record already carries both (the write is stamped,
// and the status stamps when the run was stopped under it).
type Kill struct {
	Reason string `json:"reason,omitempty"`
}

// Halter is the run-level halt an operator's kill engages: the enforcement state at the
// dispatch waist that refuses every action a halted run attempts. The reconciler holds
// one so a kill takes effect inside the step that is running, rather than waiting for it
// to finish and be observed.
//
// It is a port with two methods because a halt nobody can prove is worse than no halt at
// all. The reference harness this rule was written against shipped a kill switch that
// showed as loaded and did nothing, because the contract it emitted was dropped silently
// downstream; every run under it looked braked and none of them were. So a composition
// that wires a halter asks it, in-process and against its own enforcement path, to
// demonstrate that a halted run is actually refused, and refuses to assemble if it
// cannot. That is the same standard the evidence gate is held to (see NewEvidenceGate),
// applied to the other gate whose failure is invisible from the outside.
//
// Engaging is idempotent and keeps the first reason, so a reconcile that reads the same
// order twice does not overwrite what stopped the run with a later account of it.
type Halter interface {
	// Engage halts run, recording reason. The reason reaches the refusal the run's next
	// action is given, so it is what the person handed the stopped run reads.
	Engage(run, reason string)
	// ProveHalts demonstrates against this halter's own enforcement path that a run it
	// has halted is refused and a run it has not is not, and returns an error naming
	// what it caught if either is untrue. It runs at composition time and takes no
	// context because there is nothing to cancel: it is synchronous, in-process, and
	// touches no run that exists.
	ProveHalts() error
}

// WithHalt gives the reconciler the halt an operator's kill engages, so a kill reaches
// the waist inside the running step instead of at its boundary.
//
// Without one a kill still stops the goal: the reconciler settles it as killed, no
// further step is dispatched, and the record says who stopped it. What is lost is the
// promptness, which is the whole reason the operator reached for a kill rather than
// waiting. A host that wires no halter gets the honest weaker behaviour rather than a
// stall, because unlike a missing judge there is nothing here the goal cannot decide for
// itself.
func WithHalt(h Halter) Option { return func(g *Reconciler) { g.halt = h } }

// ValidateKillGiven refuses a kill removed from the spec after the run was stopped under
// it. A goal nobody killed, and a kill still on the spec, both pass.
func (s Status) ValidateKillGiven(k *Kill) error {
	if s.Killed == nil || k != nil {
		return nil
	}
	return ErrKillWithdrawn
}

// KillMessage renders an operator's kill as the message the stopped goal carries: that a
// person stopped it, and what they said about why if they said anything.
func KillMessage(k Kill) string {
	msg := "stopped by the operator"
	if reason := strings.TrimSpace(k.Reason); reason != "" {
		msg += ": " + reason
	}
	return msg
}

// applyKill stops a goal its operator has killed, and reports whether it handled the
// reconcile. It sits directly after admission and above everything else a reconcile does,
// including observing the step in flight, because a kill outranks the state of the work:
// a run that is mid-step, parked on its children, or one verification away from
// converging is stopped the same way, and none of those is a reason to take another look
// first.
//
// The halt is engaged before the record is written, and that order is deliberate. The
// write is what makes the stop durable and the engage is what makes it prompt, so doing
// the prompt half first means a crash in between leaves a run halted in memory against a
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
	// The step in flight is abandoned rather than awaited. Its actions are being refused
	// from here, so what is left of it is a job that will fail on its next call, and a
	// goal that went on recording it as in flight would read as still working.
	status.InFlight = nil
	status.WaitingSince = nil
	status.stall(KilledReason, msg, g.clk.Now())
	res, err := g.terminal(ctx, r, *status, specHash)
	return res, true, err
}
