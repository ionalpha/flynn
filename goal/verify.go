package goal

import (
	"context"

	"github.com/ionalpha/flynn/resource"
)

// The producer side of the evidence gate. evidence.go is the rule a claim is judged
// against; this is where the thing being judged comes from.
//
// The split across two ports is the point. ItemVerifier runs a check and says what
// happened; Evidence writes that onto the run's durable record and reads it back. Neither
// marks anything proven (the reconciler does, by folding the record into the ledger), so
// there is no path from "a check ran" to "an item is done" that skips the gate.
//
// The shape is not new. learn.Verdict is already two-axis ({Verified, Ran}), with
// learn.SandboxVerifier running the check in a fresh sandbox and learn.GovernedVerifier
// routing it through the dispatch waist so a verification is attributed to the same scope
// as the work that proposed it. That is the executed path, already built once and in
// production. This is the same shape aimed at a ledger item instead of a skill.

// ItemVerdict is the outcome of running one ledger item's declared check. Passed is the
// verdict; Executed reports whether the check actually ran.
//
// Executed is stamped by the verifier, on its own evidence that it ran the action, and
// there is no path by which a model can set it. That is the invariant the whole provenance
// axis rests on: a model that could claim execution would make the field certify nothing,
// and every rule built on it (the gate's refusal, a regrade's eligibility, a report's
// honest mix of proven-by-execution and proven-by-assertion) would collapse into
// decoration. A verifier that cannot run the check reports Executed false rather than
// guessing, exactly as learn.Verdict.Ran does.
type ItemVerdict struct {
	Passed   bool
	Executed bool
	// ExitCode is the check's process exit status and Output its raw output, recorded
	// alongside an executed verdict as the evidence of what the run actually observed.
	// Both are zero on a verdict nothing was run for, which has no execution to describe.
	ExitCode int
	Output   string
	// Detail is a short human-readable reason, shown to the next build step when a check
	// did not pass so the agent is told what its own declared check reported.
	Detail string
}

// ItemVerifier runs a ledger item's declared verify clause and reports the verdict. It is
// the executed half of the evidence gate: the item's check, run rather than asserted.
//
// A verifier that cannot run the clause is not an error. A clause no mechanism can
// execute is a real and common outcome (it is what a badly written verify clause looks
// like), and it must surface as an unexecuted verdict that the gate can then refuse under
// its execution policy, rather than as a failure that stalls the goal on the retry ladder
// or, worse, as a pass. Only a cancelled context is a hard error.
type ItemVerifier interface {
	VerifyItem(ctx context.Context, r resource.Resource, item LedgerItem) (ItemVerdict, error)
}

// Evidence is the run's durable record of item verifications: the producer writes a
// verdict to it, and the reconciler reads the verdicts back to settle the ledger.
//
// Both halves are on one port because they are one contract (what Record writes, Recorded
// must return, with a Ref stable enough to be consumed exactly once), and splitting them
// would let an implementation satisfy each half against a different record.
//
// It is a port rather than a concrete spine writer because the goal package must not
// depend on a log implementation, and because the identity of a run's stream is the
// composition's business, not the reconciler's. The shipped implementation is
// evidence.SpineEvidence.
type Evidence interface {
	// Record writes one verification of item onto r's durable record and returns it as
	// the gate will later read it, including the Ref that identifies it. The provenance
	// written is the verdict's own Executed field; an implementation must not accept a
	// provenance from anywhere else.
	Record(ctx context.Context, r resource.Resource, item string, v ItemVerdict) (Verification, error)
	// Recorded returns every item verification on r's record, in the order recorded.
	Recorded(ctx context.Context, r resource.Resource) ([]Verification, error)
}
