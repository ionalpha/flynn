package goal

// Parking and waking: how long a goal waiting on a fan-out sits before it re-checks
// itself, and how a settling child cuts that wait short for its parent.

import (
	"context"
	"errors"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/resource"
)

// recheckAfter is how long a parked goal waits before re-checking without a wake.
func (g *Reconciler) recheckAfter() time.Duration {
	if g.waitRecheck > 0 {
		return g.waitRecheck
	}
	return DefaultWaitRecheckFactor * g.poll
}

// wakeOwner clears the controller owner's park and signals it, so a parent goal
// waiting on a fan-out re-checks its children the moment one settles instead of
// waiting out the recheck fallback. Best effort: a lost wake costs latency (the
// fallback catches it), never correctness, so every failure here is dropped. The
// conflict retry matters because the parent's status is also written by its own
// reconciler and worker; losing every race would silently downgrade the wake.
func (g *Reconciler) wakeOwner(ctx context.Context, r resource.Resource) {
	owner, ok := r.Controller()
	if !ok || owner.Kind != Kind {
		return
	}
	if _, err := resource.UpdateByID(ctx, g.store, owner.ID, func(o *resource.Resource) error {
		status, err := DecodeStatus(*o)
		if err != nil || status.WaitingSince == nil {
			return resource.ErrSkipUpdate // not parked; the signal alone suffices
		}
		status.WaitingSince = nil
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		o.Status = enc
		return nil
	}); err != nil && !errors.Is(err, resource.ErrConflict) {
		return // owner gone or unreadable; nothing to wake
	}
	if g.bus != nil {
		_ = g.bus.Publish(ctx, bus.Message{Subject: StepSubject, Payload: []byte(owner.ID)})
	}
}
