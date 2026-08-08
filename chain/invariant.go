package chain

// The invariant vocabulary a run records on its spine: an audit of one term of the run,
// ruling on whether it still holds. Where ItemVerified answers "is this piece of work
// done", this answers "is this still true", and the two are separate event types because
// a reader looking for one must never be handed the other. An item's verification is
// spent once, against the item it proves; a term's audit is a standing observation that
// is made again after every step.
//
// The record matters here for a reason it does not for an item. A term is what a run is
// held to when nobody is watching, so the proof that it was actually checked has to
// survive the run: a status field saying an audit happened is the run's own account of
// itself, and this is the run's log saying so with the exit code and output hash of the
// check that was run.
const (
	// InvariantAudited is one term of the run, audited. The term is InvariantKey and the
	// verdict is InvariantHeldKey; a term found broken also carries InvariantDetailKey
	// saying what was observed.
	InvariantAudited = "invariant.audited"
	// InvariantKey holds the id of the term an InvariantAudited event pertains to.
	InvariantKey = "invariant"
	// InvariantHeldKey holds the verdict: true when the term still holds. A false
	// verdict is the breach, and it is recorded rather than left to the status alone,
	// because a run that broke its terms is exactly the run whose own account of itself
	// is worth the least.
	InvariantHeldKey = "held"
	// InvariantDetailKey holds what the audit observed about a term it found broken. It
	// is what gets quoted to whoever is handed the stopped goal, so it says what
	// happened rather than that something happened.
	InvariantDetailKey = "detail"
	// InvariantCitedKey holds what an audit that ran no command read to reach its
	// verdict: the part of the run's record the auditor is pointing at. A verdict with
	// nothing cited is refused rather than recorded, so this key is present on every
	// asserted audit, and a reader can go back to what was actually looked at instead
	// of taking the verdict's word for it. An executed audit does not carry it: the
	// command line and its exit code are the citation.
	InvariantCitedKey = "cited"
)

// An audit reuses the item vocabulary's provenance axis (ItemProvenanceKey, ItemExitKey,
// ItemOutputKey) rather than growing a second one. The distinction being drawn is
// identical, a verdict reached by running something against a verdict merely asserted,
// and two vocabularies for it would be two ways to say executed, with every reader
// having to know both.
