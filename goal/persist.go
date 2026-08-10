package goal

// Every write the reconciler makes to a goal record, and the conflict policy each
// one needs. A settled status is written blind; an in-flight reservation races the
// step's own worker and is reapplied onto a fresh read.

import (
	"context"
	"errors"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// recordDispatch persists the in-flight reservation for a step that was just
// enqueued. Unlike a settled-status write, it must survive a race with the step's
// own worker: the job is enqueued (Enqueue, above) before this write runs, so a
// worker on a tight poll can claim it, take its turn, and persist that turn's
// checkpoint before this write lands. A blind Put would then lose the optimistic
// race and be dropped, and the dropped InFlight marker means the completed step is
// never observed in the next pass and never counted against the step budget, so an
// extra turn runs past MaxSteps (the goal converges where it must stall). Retrying
// the whole reconcile does not recover it: the retry re-reads a state that has
// already lost the job-to-reservation link. So this reapplies the reservation onto
// a fresh read with the shared conflict-retry policy instead. Only reconciler-owned
// fields are written; the worker-owned checkpoint and waiting mark are carried over
// from the fresh record so neither writer clobbers the other.
func (g *Reconciler) recordDispatch(ctx context.Context, r resource.Resource, status Status, specHash string) error {
	status.ObservedSpecHash = specHash
	_, err := resource.UpdateByID(ctx, g.store, r.ID, func(fresh *resource.Resource) error {
		cur, err := DecodeStatus(*fresh)
		if err != nil {
			return err
		}
		status.Checkpoint = cur.Checkpoint
		status.WaitingSince = cur.WaitingSince
		// The planning mark and the per-item state are the worker's too, and the
		// planning step is the one most likely to land inside this window: the plan
		// job is enqueued before this reservation is written, so a worker that
		// claims and finishes it first would otherwise have its ledger erased here
		// and be asked to plan the same goal all over again.
		status.Planned = cur.Planned
		// The ledger has two writers: the worker appends items when it plans, and the
		// reconciler marks them proven when the record backs them. So it is merged
		// rather than taken from either side. Carrying the worker's copy wholesale
		// would drop a proof this very pass admitted; carrying ours would drop an item
		// a concurrent planning step just appended.
		var proved bool
		status.Ledger, proved = mergeLedger(status.Ledger, cur.Ledger)
		// The failing check's detail is the worker's, and a verify job lands in this
		// same window: it is enqueued before this reservation is written, so a worker
		// that claims and finishes it first would otherwise have it erased here, and the
		// next build step would be asked to fix an item without being told what its own
		// check reported. The exception is a pass that just proved something, which is
		// the pass that cleared the feedback on purpose.
		if !proved {
			status.ItemFeedback = cur.ItemFeedback
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

// mergeLedger reconciles this reconcile's per-item state with the copy on the freshest
// record, and reports whether the merge carried a proof the record did not already have.
//
// The shape of the ledger comes from theirs: it is the copy written against the newest
// version of the goal, so a planning step that appended items in this window is respected
// and this write does not shorten the plan. The proven marks come from whichever side has
// them, because a proof only ever goes from unset to set and never back. An item that is
// proven on either copy is proven, and the earlier proof's evidence and timestamp are kept
// intact rather than restamped.
func mergeLedger(mine, theirs []LedgerState) ([]LedgerState, bool) {
	proofs := make(map[string]LedgerState, len(mine))
	for _, st := range mine {
		if st.Proven {
			proofs[st.ID] = st
		}
	}
	added := false
	out := make([]LedgerState, len(theirs))
	copy(out, theirs)
	for i, st := range out {
		if st.Proven {
			continue
		}
		if p, ok := proofs[st.ID]; ok {
			out[i] = p
			added = true
		}
	}
	return out, added
}

// persistStatus records the observed spec hash and persists the status via the
// store's optimistic-concurrency Put.
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

// terminal persists a settled status (converged or stalled) and requests no
// requeue: the goal has reached a steady state, so it is only revisited when its
// spec changes or at the next resync, not on a timer.
func (g *Reconciler) terminal(ctx context.Context, r resource.Resource, status Status, specHash string) (reconcile.Result, error) {
	if err := g.persistStatus(ctx, r, status, specHash); err != nil {
		return reconcile.Result{}, err
	}
	g.wakeOwner(ctx, r)
	return reconcile.Result{}, nil
}

// putErr maps a write conflict to a Transient error so the controller backs off
// and retries with a fresh read, rather than treating a lost race as fatal.
func putErr(err error) error {
	if errors.Is(err, resource.ErrConflict) {
		return fault.Wrap(fault.Transient, "goal_write_conflict", err)
	}
	return err
}
