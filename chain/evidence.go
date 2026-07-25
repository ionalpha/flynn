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
)
