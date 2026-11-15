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

// KilledReason is its own reason, because only a run a person stopped has somebody to ask.
const KilledReason = "Killed"

// Kill is on the spec so it reaches a run another process drives. It is the order and
// [Halter] the enforcement, kept apart so the record says who stopped the run. The step in
// flight ends at its next model or tool call; a running sandbox command runs to its limit.
//
// Removing a kill is [ErrKillWithdrawn], so the record never reads as a run that stopped
// on its own. Nothing branches on Reason.
type Kill struct {
	Reason string `json:"reason,omitempty"`
}

// Halter refuses every dispatch action of a halted run, so a kill acts inside the running
// step. ProveHalts exists because an unproven halt is worse than none; assembly refuses a
// halter that fails it, as with [NewEvidenceGate]. Engage keeps the first reason.
type Halter interface {
	// Engage's reason is what the next refused action reports.
	Engage(run, reason string)
	// ProveHalts checks its own enforcement path refuses a halted run and passes others.
	// It runs in-process at composition and touches no real run, so it takes no context.
	ProveHalts() error
}

// WithHalt makes a kill act inside the running step. Without it a kill still settles the
// goal after the step, and nothing stalls, because the goal can decide a kill alone.
func WithHalt(h Halter) Option { return func(g *Reconciler) { g.halt = h } }

// ValidateKillGiven refuses a kill removed from the spec after the run was stopped under
// it. A goal nobody killed, and a kill still on the spec, both pass.
func (s Status) ValidateKillGiven(k *Kill) error {
	if s.Killed == nil || k != nil {
		return nil
	}
	return ErrKillWithdrawn
}

// KillMessage is the message a killed goal carries.
func KillMessage(k Kill) string {
	msg := "stopped by the operator"
	if reason := strings.TrimSpace(k.Reason); reason != "" {
		msg += ": " + reason
	}
	return msg
}

// applyKill runs right after admission, before observing the step, because a kill
// outranks the state of the work. The halt engages before the write, so a crash between
// leaves a halted run the next pass settles; the other order leaves a settled record over
// a run still acting.
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
	// Abandoned without waiting, because its actions are refused and an in-flight mark
	// would read as working.
	status.InFlight = nil
	status.WaitingSince = nil
	return g.stalled(ctx, r, status, specHash, KilledReason, msg)
}
