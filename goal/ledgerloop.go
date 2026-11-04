package goal

// The run-path half of the ledger loop: settling items against the durable record,
// and holding a completion claim to what that record actually proves.

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

// ledgerGated reports whether this reconciler closes the ledger loop: it has both a record
// to read verifications from and a gate to judge them with. The two are set together, so
// this is one question rather than two, and every ledger-loop branch asks it in one place
// instead of re-deriving the condition.
func (g *Reconciler) ledgerGated() bool { return g.evidence != nil && g.gate != nil }

// settleLedger folds the run's own record back into its ledger: every unproven item the
// evidence gate admits flips to proven, consuming the verification that proved it. It
// returns the verifications it read, so a completion refused moments later can name why
// each remaining item is unproven without reading the record twice.
//
// This is the only path to a proven item on the run path. It reads the durable record
// rather than trusting a claim, which is what makes the per-item state a projection of the
// spine instead of a second opinion about it.
func (g *Reconciler) settleLedger(ctx context.Context, r resource.Resource, status *Status) ([]Verification, error) {
	if !g.ledgerGated() || !status.Planned || len(status.Unproven()) == 0 {
		return nil, nil
	}
	recorded, err := g.evidence.Recorded(ctx, r)
	if err != nil {
		return nil, err
	}
	if status.ProveRecorded(g.gate, recorded, g.clk.Now()) > 0 {
		// The feedback describes a check that failed on an item now proven or moved
		// past, so it must not ride into the next step.
		status.ItemFeedback = ""
		status.MissingCommand, status.MissingRepeat = "", 0
	}
	return recorded, nil
}

// holdsClaimAgainstLedger reports whether this goal's completion claim has to answer to
// its ledger. An unplanned goal, or one whose ledger is empty, does not: LedgerSettled is
// false for an empty ledger, so without this such a goal could never converge at all.
func (g *Reconciler) holdsClaimAgainstLedger(spec Spec, status Status) bool {
	return g.ledgerGated() && g.ledgerConverge &&
		status.Planned && len(spec.Ledger) > 0 && !status.LedgerSettled()
}

// claimOverOpenItem reports whether a completion claim in a run that does not require
// proof has to wait for the ledger: an item is still neither proven nor moved past. Such a
// run's completion is the model's call once every item has been tried, never before. A
// model briefed on one item says it is done when that item is, and accepting that would
// end the run with the rest of the plan never attempted. Each open item is still bounded:
// a check that keeps failing the same way moves the run past it (moveOffUnprovableItem),
// and the step budget bounds the rest.
func (g *Reconciler) claimOverOpenItem(spec Spec, status Status) bool {
	if g.ledgerConverge || !g.ledgerGated() || !status.Planned {
		return false
	}
	_, open := status.CurrentItem(spec.Ledger)
	return open
}

// moveOffUnrunnableItem abandons the current item when the run has claimed completion and
// the item's latest check, taken after its last build, could not be run at all (the
// sandbox refused it, or there was no check). Holding the claim for such an item teaches
// the run nothing: the host refuses the same check the same way every time, and a check
// that never ran is never counted as a repeat, so the run would spin until the no-progress
// guard stalled it. Only a claim reaches here, so an item the model is still working on
// keeps its chance. recorded is the run's verifications in recorded order.
func (g *Reconciler) moveOffUnrunnableItem(spec Spec, status *Status, recorded []Verification) {
	if status.VerifyPending {
		return // the latest check predates the last build; it says nothing about the work now
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

// refuseCompletion settles a goal whose model reported success over an unproven ledger,
// naming each unproven item and the reason the gate would refuse it.
//
// This is the line that must not be softened. An "unless" here (a grace pass, an
// override, a claim trusted because its check was awkward to run) restores exactly the
// prose completion the ledger replaced.
func (g *Reconciler) refuseCompletion(status *Status, recorded []Verification) {
	reasons := status.UnprovenReasons(g.gate, recorded)
	status.stall("LedgerUnproven", fmt.Sprintf("completion reported with %d planned item(s) unproven: %s",
		len(reasons), strings.Join(reasons, "; ")), g.clk.Now())
}

// moveOffUnprovableItem abandons the current item, in a run that does not require proof,
// once its check has failed the same way twice or named a missing program twice. feedback
// is the executed check failure this cycle was counted on, or "" when none ran.
//
// The checks are the model's own, written before any work, and some can never pass (a
// quoting mistake) or check for a file the objective never asked for. Left to the
// non-convergence guard, either one stops a run whose ledger the operator was told is
// only a record. The item stays unproven, with the failure that gave it up.
//
// A run that requires proof stops on the item instead, and a repeated refusal with no
// check failure in it is still the non-convergence stop.
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
