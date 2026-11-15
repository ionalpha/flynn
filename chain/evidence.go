package chain

// The per-item evidence vocabulary. Where CheckRecorded and OutcomeRecorded
// (groundtruth.go) ground a whole run's result, this grounds one ledger item. The
// evidence gate (goal.EvidenceGate) will not mark an item proven without one.
//
// These strings are the wire contract a producer writes and the gate reads.
// ItemVerified is distinct from CheckRecorded because they answer different
// questions (item proven, run outcome grounded) and neither verifier should have
// to guess which one a shared event meant.
const (
	// ItemVerified is the verdict of a verification against one ledger item, named
	// by ItemKey, with the verdict in ItemPassedKey. The gate consumes the event's
	// spine sequence as its identity, so one verification cannot certify two items.
	ItemVerified = "item.verified"
	// ItemKey holds the ledger item id, the content address of the item's text and
	// verify clause, so it names the exact check committed to at planning time.
	ItemKey = "item"
	// ItemPassedKey holds the boolean verdict. A false verdict is recorded: a
	// verification that ran and failed is evidence the item is not done, not a
	// missing attempt.
	ItemPassedKey = "passed"
	// ItemProvenanceKey holds ProvenanceExecuted when the check was run and
	// ProvenanceAsserted when it was not. They are different kinds of evidence, and
	// only the executed kind can be re-adjudicated by a later regrade.
	//
	// A producer must stamp this itself, having run the action. A model must have no
	// path to write it: if it could claim ProvenanceExecuted the field would certify
	// nothing.
	ItemProvenanceKey = "provenance"
	// ItemExitKey holds the exit code of an executed check and ItemOutputKey the
	// hash of its output, so a reader can tell a check that passed from one said to
	// have passed. Both are absent on an asserted verification.
	ItemExitKey   = "exit"
	ItemOutputKey = "outputHash"
	// ItemReasonKey holds why no check was run: no clause, no sandbox, a gate
	// refused it, or the command could not start. Without it "could not be run"
	// names no cause, so a fixable failure looks the same as a check that never
	// could run on this host. It is absent on an executed verification.
	ItemReasonKey = "reason"
)

// The admission vocabulary. Before work starts each item's check is admitted or
// refused (goal.CheckGrounding, goal.DryRunRefusal), so a reader can tell a vetted
// check from one taken on trust. ItemKey names the item and ItemReasonKey carries
// the sentence the planner was sent.
const (
	// ItemAdmitted is the admission verdict for one item's check. It is not
	// evidence of completion and the evidence gate never reads it.
	ItemAdmitted    = "item.admitted"
	ItemAdmittedKey = "admitted"
	// ItemRefusalKey holds the stable name of the refusal, absent when admitted.
	ItemRefusalKey = "refusal"
	// ItemDryRunKey is true when the check ran once on the untouched tree before
	// admission, false when admitted on its text alone.
	ItemDryRunKey = "dryRun"
)

// The two kinds of item evidence. Neither converts into the other; nothing
// promotes an assertion into an execution.
const (
	// ProvenanceExecuted marks a verdict reached by running the declared check,
	// with exit code and output hash on the same event.
	ProvenanceExecuted = "executed"
	// ProvenanceAsserted marks a verdict nothing was run for. A reader must also
	// take a missing or unreadable provenance as this, the safe default for a gate
	// that fails closed.
	ProvenanceAsserted = "asserted"
)
