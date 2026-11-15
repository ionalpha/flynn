package goal

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

func (g *Reconciler) ledgerGated() bool { return g.evidence != nil && g.gate != nil }

// settleLedger is the only run-path route to a proven item, and it reads the durable
// record, so item state is a projection of the spine. It returns what it read so a refused
// completion can explain each open item without a second read.
func (g *Reconciler) settleLedger(ctx context.Context, r resource.Resource, status *Status) ([]Verification, error) {
	if !g.ledgerGated() || !status.Planned || len(status.Unproven()) == 0 {
		return nil, nil
	}
	recorded, err := g.evidence.Recorded(ctx, r)
	if err != nil {
		return nil, err
	}
	if status.ProveRecorded(g.gate, recorded, g.clk.Now()) > 0 {
		// The feedback is about an item now past, so it must not reach the next step.
		status.ItemFeedback = ""
		status.MissingCommand, status.MissingRepeat = "", 0
	}
	return recorded, nil
}

// holdsClaimAgainstLedger excludes an empty ledger, because LedgerSettled is false for one
// and the goal could never converge.
func (g *Reconciler) holdsClaimAgainstLedger(spec Spec, status Status) bool {
	return g.ledgerGated() && g.ledgerConverge &&
		status.Planned && len(spec.Ledger) > 0 && !status.LedgerSettled()
}

// claimOverOpenItem holds a claim in a run without required proof until every item was
// tried, because a model briefed on one item says it is done when that item is.
// moveOffUnprovableItem and the step budget bound each open item.
func (g *Reconciler) claimOverOpenItem(spec Spec, status Status) bool {
	if g.ledgerConverge || !g.ledgerGated() || !status.Planned {
		return false
	}
	_, open := status.CurrentItem(spec.Ledger)
	return open
}

// moveOffUnrunnableItem abandons a claimed item whose latest check could not run, because
// an unrun check never counts as a repeat and the run would spin until the no-progress
// stop. Only a claim reaches here. recorded is in recorded order.
func (g *Reconciler) moveOffUnrunnableItem(spec Spec, status *Status, recorded []Verification) {
	if status.VerifyPending {
		return // the latest check predates the last build
	}
	item, ok := status.CurrentItem(spec.Ledger)
	if !ok {
		return
	}
	for i := len(recorded) - 1; i >= 0; i-- {
		if recorded[i].Item != item.ID {
			continue
		}
		if recorded[i].Provenance != ProvenanceExecuted {
			detail := status.ItemFeedback
			if detail == "" {
				detail = recorded[i].Reason
			}
			status.abandonCurrent(spec.Ledger, "its check could not be run: "+strings.TrimSpace(detail))
		}
		return
	}
}

// refuseCompletion names each unproven item and its refusal. Do not add an exception here
// (a grace pass, an override): any one restores completion by prose claim.
func (g *Reconciler) refuseCompletion(status *Status, recorded []Verification) {
	reasons := status.UnprovenReasons(g.gate, recorded)
	status.stall("LedgerUnproven", fmt.Sprintf("completion reported with %d planned item(s) unproven: %s",
		len(reasons), strings.Join(reasons, "; ")), g.clk.Now())
}

// moveOffUnprovableItem abandons an item whose check failed the same way twice, in a run
// without required proof. The model writes its checks before any work and some can never
// pass, which would otherwise stop a run whose ledger is only a record. The item stays
// unproven. feedback is the executed failure counted this cycle, or "".
func (g *Reconciler) moveOffUnprovableItem(spec Spec, status *Status, feedback string) {
	if g.ledgerConverge || !g.ledgerGated() || !status.Planned {
		return
	}
	var reason string
	switch {
	case status.StalledForEnvironment():
		reason = status.EnvironmentReason()
	case feedback != "" && status.StalledForNonConvergence():
		reason = fmt.Sprintf("its check failed the same way on %d consecutive runs: %s", status.VerdictRepeat, feedback)
	default:
		return
	}
	status.abandonCurrent(spec.Ledger, reason)
}
