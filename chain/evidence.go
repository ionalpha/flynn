package chain

// The per-item evidence vocabulary a run records on its spine. Where the ground-truth
// vocabulary (CheckRecorded/OutcomeRecorded, groundtruth.go) grounds a whole run's
// claimed result, this grounds one ledger item: the run ran the item's declared
// verification and recorded the verdict. The evidence gate (goal.EvidenceGate) reads
// these events and will not flip a ledger item to proven without one.
//
// These string values are the wire contract a producer writes and the gate reads. An
// item-scoped verification is a distinct event type from CheckRecorded on purpose: the
// two answer different questions (is this item proven vs is the run's outcome
// grounded), and keeping them separate means neither verifier has to guess which one a
// shared event meant.
const (
	// ItemVerified is the verdict of a verification run against a single ledger item.
	// The item it pertains to is ItemKey; its boolean verdict is ItemPassedKey. The
	// event's own spine sequence is its identity, which is what the gate consumes so one
	// recorded verification cannot certify two items.
	ItemVerified = "item.verified"
	// ItemKey holds the ledger item id an ItemVerified event pertains to. The id is the
	// content address of the item's text and its declared verify clause, so naming the
	// id is naming the exact check the item committed to at planning time.
	ItemKey = "item"
	// ItemPassedKey holds an ItemVerified event's boolean verdict. A false verdict is
	// recorded, not omitted: a verification that ran and failed is evidence the item is
	// not done, and the gate refuses it as such rather than treating it as no attempt.
	ItemPassedKey = "passed"
	// ItemProvenanceKey holds how the verdict was arrived at: ProvenanceExecuted when
	// the check was actually run, ProvenanceAsserted when it was not. It is a second
	// axis, not a confidence score, because the two are different kinds of evidence and
	// only the executed kind can be re-adjudicated by a later regrade.
	//
	// A producer must stamp this itself, on the evidence that it ran the action. A model
	// cannot be given a path to write it: if it could claim ProvenanceExecuted the field
	// would certify nothing, and every rule built on it would be decoration.
	ItemProvenanceKey = "provenance"
	// ItemExitKey holds the exit code of an executed check, and ItemOutputKey the hash of
	// its output. They are the executed case's own evidence: what the run actually
	// observed, recorded alongside the verdict so a later reader can tell a check that
	// passed from one that was merely said to have passed. Both are absent on an asserted
	// verification, which has no execution to describe.
	ItemExitKey   = "exit"
	ItemOutputKey = "outputHash"
	// ItemReasonKey holds why a verdict nothing was run for was not run: no clause to
	// run, no sandbox to run it in, a gate that refused it, a command that could not
	// start. It is the unexecuted case's counterpart to the exit code, and it is on the
	// record for the same reason the exit code is.
	//
	// Without it the record says an item is unproven and stops there, and "its check
	// could not be run" is the one outcome a reader can do nothing with: it names no
	// cause, so it is indistinguishable from a check that failed for a reason worth
	// fixing and one that was never going to run on this host. It is absent on an
	// executed verification, whose exit code and output hash are what happened.
	ItemReasonKey = "reason"
)

// The two kinds of item evidence. They are kinds, not tiers: neither is convertible
// into the other, and there is no path that promotes an assertion into an execution.
const (
	// ProvenanceExecuted marks a verdict the producer reached by running the item's
	// declared check, with the exit code and output hash on the same event.
	ProvenanceExecuted = "executed"
	// ProvenanceAsserted marks a verdict nothing was run for: the model said so. It is
	// also how a reader must take a verdict whose provenance is missing or unreadable,
	// since the weakest reading is the only safe default for a gate that fails closed.
	ProvenanceAsserted = "asserted"
)
