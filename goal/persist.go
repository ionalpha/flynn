package goal

import (
	"context"
	"errors"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// recordDispatch reapplies the reservation onto a fresh read, because the job is enqueued
// first and its worker can persist a checkpoint before this lands. A blind Put would lose
// that race and drop InFlight, so the step goes uncounted and a turn runs past MaxSteps;
// retrying the reconcile cannot recover the link. Worker-owned fields come from the fresh
// record so neither writer clobbers the other.
func (g *Reconciler) recordDispatch(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	status.ObservedSpecHash = specHash
	_, err := resource.UpdateByID(ctx, g.store, r.ID, func(fresh *resource.Resource) error {
		cur, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		status.Checkpoint = cur.Checkpoint
		status.WaitingSince = cur.WaitingSince
		// The planning step most often lands in this window, and taking ours would
		// erase its ledger and ask for the plan again.
		status.Planned = cur.Planned
		// Merged because both write it: the worker appends items and this pass may have
		// proven one.
		var settled bool
		status.Ledger, settled = mergeLedger(status.Ledger, cur.Ledger)
		// A verify job can land in this window too, so its feedback is kept unless this
		// pass settled an item and cleared it on purpose.
		if !settled {
			status.ItemFeedback = cur.ItemFeedback
			status.MissingCommand, status.MissingRepeat = cur.MissingCommand, cur.MissingRepeat
		}
		enc, err := status.Encode()
		if err != nil {
			return err
		}
		fresh.Status = enc
		return nil
	})
	if err != nil {
		return putErr(err)
	}
	return nil
}

// mergeLedger takes the item list from theirs, so appended items survive, and a proof or
// abandon mark from either side, because neither is ever unset. An earlier proof keeps its
// evidence and time. It reports whether it carried a mark, so the caller does not restore
// feedback that would read as the next item's first failure.
func mergeLedger(mine, theirs []LedgerState) ([]LedgerState, bool) {
	settled := make(map[string]LedgerState, len(mine))
	for _, st := range mine {
		if st.Proven || st.Abandoned {
			settled[st.ID] = st
		}
	}
	added := false
	out := make([]LedgerState, len(theirs))
	copy(out, theirs)
	for i, st := range out {
		if st.Proven {
			continue
		}
		p, ok := settled[st.ID]
		switch {
		case !ok:
		case p.Proven:
			out[i] = p
			added = true
		case !st.Abandoned:
			out[i].Abandoned, out[i].AbandonedReason = true, p.AbandonedReason
			added = true
		}
	}
	return out, added
}

func (g *Reconciler) persistStatus(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	status.ObservedSpecHash = specHash
	enc, err := status.Encode()
	if err != nil {
		return fault.Wrap(fault.Terminal, "goal_status_encode", err)
	}
	r.Status = enc
	if _, err := g.store.Put(ctx, r); err != nil {
		return putErr(err)
	}
	return nil
}

// stalled is where every gate that stops a run ends.
func (g *Reconciler) stalled(ctx context.Context, r resource.Resource, status *Status, specHash, reason, message string) (reconcile.Result, bool, error) {
	status.stall(reason, message, g.clk.Now())
	res, err := g.terminal(ctx, r, *status, specHash)
	return res, true, err
}

// terminal requests no requeue, so a settled goal is revisited only on a spec change or
// resync.
func (g *Reconciler) terminal(ctx context.Context, r resource.Resource, status Status, specHash string) (reconcile.Result, error) {
	if err := g.persistStatus(ctx, r, status, specHash); err != nil {
		return reconcile.Result{}, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, nil
}

// putErr makes a conflict Transient so the controller retries on a fresh read.
func putErr(err error) error {
	if errors.Is(err, resource.ErrConflict) {
		return fault.Wrap(fault.Transient, "goal_write_conflict", err)
	}
	return err
}
