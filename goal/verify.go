package goal

import (
	"context"

	"github.com/ionalpha/flynn/resource"
)

// ItemVerdict is one ledger check's outcome. Only the verifier sets Executed, on its own
// evidence, and no model can, because every provenance rule depends on it. A verifier that
// cannot run the check reports false.
type ItemVerdict struct {
	Passed   bool
	Executed bool
	// ExitCode and Output are zero when nothing ran.
	ExitCode int
	Output   string
	// Missing is a program the host lacks, set with Executed false. The worker sets it
	// from the shell's not-found exit; a verifier need not.
	Missing string
	// Detail is shown to the next build step after a failure.
	Detail string
}

// ItemVerifier runs a ledger item's check. Neither it nor [Evidence] marks anything
// proven, so no path skips the gate. An unrunnable clause is common and returns an
// unexecuted verdict for the gate to judge, because an error would stall the goal on
// retries. Only a cancelled context is an error.
type ItemVerifier interface {
	VerifyItem(ctx context.Context, r resource.Resource, item LedgerItem) (ItemVerdict, error)
}

// Evidence is the durable record of verifications. Both methods share one port, because
// Recorded must return what Record wrote with a Ref consumed exactly once. It is a port so
// goal does not depend on a log; evidence.SpineEvidence implements it.
type Evidence interface {
	// Record returns the verification as the gate will read it. Provenance must come only
	// from v.Executed.
	Record(ctx context.Context, r resource.Resource, item string, v ItemVerdict) (Verification, error)
	// Recorded returns verifications in recorded order.
	Recorded(ctx context.Context, r resource.Resource) ([]Verification, error)
}
