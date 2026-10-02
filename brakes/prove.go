package brakes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// A brake that is wired but not enforcing is the worst of the three states it can be in,
// because it is the one that is trusted. Nothing observably changes: the run dispatches
// its actions, the halt state records that it is halted, and the two never meet. The
// harness this brake is modeled against shipped exactly that, a kill switch that reported
// itself loaded while doing nothing, because the contract it emitted had been deprecated
// downstream and nobody checked.
//
// So the halt is provable, in-process, against the code that actually enforces it. It is
// the same standard the evidence gate is held to and it is here for the same reason: a
// gate whose failure mode is silence has to demonstrate that it works before anything is
// allowed to rely on it, and the demonstration has to drive the real path rather than a
// description of it.

// ErrBrakesBroken reports that the brake's halt did not behave: an action on a halted run
// was let through, an action on an unhalted one was refused, the refusal was classified as
// something a caller would retry, or a reset did not release the run. A brake that cannot
// prove it halts must not be wired, because the run under it would look braked and not be.
var ErrBrakesBroken = errors.New("brakes: halt self-test failed")

// proofRun is the run id ProveHalts halts and releases. It is not a run: nothing dispatches
// under it, and the bookkeeping the proof leaves behind is dropped before it returns, so
// proving costs one map entry that does not outlive the call.
const proofRun = "brakes.halt-proof"

// proofAction is the action name the proof dispatches. It is never admitted or executed;
// Before is asked about it and the answer is the whole result.
const proofAction = "brakes.halt-proof"

// proofReason is what the proof halts its probe run with. It is asserted to reach the
// refusal, because a halt whose reason is dropped leaves the person handed the stopped run
// with no account of who stopped it or why, and the operator's reason is the entire content
// of a kill.
const proofReason = "halt self-test"

// Engage halts run through this Hook's kill-switch, recording reason. It is the same state
// a breaker trips, so an operator's halt and an automatic one are one thing: once engaged,
// every action that run dispatches through this Hook is refused until it is reset.
//
// It exists alongside Switch() so a caller that only needs to stop a run holds the brake
// rather than reaching through it for the switch. That is what lets the halt be wired as a
// port: the thing handed over is the brake that enforces, not a halt state that may or may
// not be the one any waist consults.
func (h *Hook) Engage(run, reason string) { h.sw.Engage(run, reason) }

// ProveHalts demonstrates, against this Hook's own Before path, that its halt enforces.
// It asserts all four things that have to hold for a kill to mean anything: an unhalted run
// passes, a halted run is refused, the refusal is Forbidden and carries the reason it was
// halted with, and a reset releases the run again. Any deviation returns ErrBrakesBroken
// naming what it caught.
//
// The third assertion is the one that is easy to leave out and the one a caller depends on
// most. A halt reported as transient is a halt the caller retries past, so a refusal with
// the wrong class is a brake that delays a run rather than stopping it. The fourth is there
// for the opposite reason: a Before that refuses unconditionally would pass an assertion
// that halted runs are refused while halting every run there is.
//
// It takes no context because there is nothing to cancel. The path it drives is
// synchronous and in-process, it touches no run that exists, and it is called while a
// composition is being assembled, before there is a run to be canceled on behalf of.
func (h *Hook) ProveHalts() error {
	ctx := Into(context.Background(), proofRun)
	probe := dispatch.Action{Name: proofAction}
	defer h.forget(proofRun)

	// 1. Not halted: must pass. Without this the whole proof is satisfied by a brake that
	// refuses everything, which is not a brake, it is an outage.
	//
	// What the brake said is quoted rather than wrapped, here and below. A caller matches
	// on ErrBrakesBroken to know the brake is unusable; the refusal it produced is the
	// evidence of what went wrong and not a second error to be matched on.
	if err := h.Before(ctx, probe); err != nil {
		return fmt.Errorf("%w: an action on a run with no halt engaged was refused (%s)", ErrBrakesBroken, err.Error())
	}

	// 2. Halted: must be refused, 3. as a policy denial that carries the reason.
	h.sw.Engage(proofRun, proofReason)
	err := h.Before(ctx, probe)
	h.sw.Reset(proofRun)
	switch {
	case err == nil:
		return fmt.Errorf("%w: an action on a halted run was admitted", ErrBrakesBroken)
	case fault.Classify(err) != fault.Forbidden:
		return fmt.Errorf("%w: a halted run's refusal was classified %s, which a caller retries past (%s)",
			ErrBrakesBroken, fault.Classify(err), err.Error())
	case !strings.Contains(err.Error(), proofReason):
		return fmt.Errorf("%w: the halt reason did not reach the refusal (%s)", ErrBrakesBroken, err.Error())
	}

	// 4. Released: a reset must let the run dispatch again, so a halt is a state and not a
	// one-way door the run can never come back through.
	if err := h.Before(ctx, probe); err != nil {
		return fmt.Errorf("%w: a reset halt still refused the run (%s)", ErrBrakesBroken, err.Error())
	}
	return nil
}

// forget drops a run's breaker bookkeeping. Only the proof uses it: a real run's state is
// kept for the life of the Hook because a run that stops dispatching may start again, and
// the proof's probe never will.
func (h *Hook) forget(run string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, run)
}
