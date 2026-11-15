package brakes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// ErrBrakesBroken reports that [Hook.ProveHalts] caught the halt misbehaving. A brake
// that fails it must not be wired, because its run would look braked and not be.
var ErrBrakesBroken = errors.New("brakes: halt self-test failed")

// proofRun is the probe run id. Nothing dispatches under it, and its bookkeeping is
// dropped before ProveHalts returns.
const proofRun = "brakes.halt-proof"

// proofAction is only ever passed to Before; it never runs.
const proofAction = "brakes.halt-proof"

// proofReason must reach the refusal, because a stopped run's owner needs the operator's
// reason for the kill.
const proofReason = "halt self-test"

// Engage halts run through the Hook's kill-switch, the state a breaker trips. A caller
// that only stops runs holds the brake itself, so the halt it engages is the one this
// Hook enforces.
func (h *Hook) Engage(run, reason string) { h.sw.Engage(run, reason) }

// ProveHalts drives the Hook's own Before path to show the halt enforces, because a
// brake wired but not enforcing looks the same as one that works. It checks that an
// unhalted run passes, a halted run is refused as Forbidden with the halt reason, and a
// reset releases the run. A transient class would be retried past. The first and last
// checks rule out a Before that refuses everything. Any failure wraps [ErrBrakesBroken].
//
// It takes no context because it runs at assembly, before any run exists.
func (h *Hook) ProveHalts() error {
	ctx := Into(context.Background(), proofRun)
	probe := dispatch.Action{Name: proofAction}
	defer h.forget(proofRun)

	// The brake's own refusal is quoted, not wrapped, so callers match only
	// ErrBrakesBroken.
	if err := h.Before(ctx, probe); err != nil {
		return fmt.Errorf("%w: an action on a run with no halt engaged was refused (%s)", ErrBrakesBroken, err.Error())
	}

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

	if err := h.Before(ctx, probe); err != nil {
		return fmt.Errorf("%w: a reset halt still refused the run (%s)", ErrBrakesBroken, err.Error())
	}
	return nil
}

// forget drops a run's bookkeeping. Only the proof uses it, because a real run that
// goes quiet may dispatch again.
func (h *Hook) forget(run string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, run)
}
