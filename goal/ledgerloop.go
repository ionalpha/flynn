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
