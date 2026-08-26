package chain

// The steer vocabulary a run records on its spine: an operator's redirect, and the
// judgement of whether the run's account of finishing addressed it. Where InvariantAudited
// answers "is this still true", this answers "was the person who interrupted this run
// answered", and it is a separate event type for the same reason: the two are read by
// different questions and a reader looking for one must never be handed the other.
//
// It is on the record rather than only on the status because of what the status can lose.
// An acknowledgement is a judgement about one sentence the run wrote at one moment, and the
// status keeps the outcome. The event keeps the occasion: which redirect, what the run said
// for itself, what the judge made of it, and when. A run that was steered four times and
// answered three is a fact about how it was operated, and it is worth reading long after
// the status has been overwritten by the next thing the goal did.
const (
	// SteerJudged is one operator redirect, ruled on against the run's own account of
	// having finished. The redirect is SteerKey and the verdict is SteerAddressedKey; a
	// redirect the account did address also carries SteerHowKey.
	SteerJudged = "steer.judged"
	// SteerKey holds the id of the redirect a SteerJudged event pertains to.
	SteerKey = "steer"
	// SteerAddressedKey holds the verdict: true when the run's account addressed the
	// redirect. False is what refuses the run's completion, and it is recorded rather
	// than left to the status alone, because a run that ignored its operator is exactly
	// the run whose own account of itself is worth the least.
	SteerAddressedKey = "addressed"
	// SteerHowKey holds what the judge accepted as the run's answer: the part of the
	// account that addressed the redirect. A verdict of addressed with nothing quoted is
	// refused rather than recorded, so this key is present on every accepted
	// acknowledgement and a reader can go back to the run's own words instead of taking
	// the verdict's word for it.
	SteerHowKey = "how"
	// SteerAccountKey holds the account the judge was ruling on: the run's statement of
	// what it did, verbatim. It is kept because a refused acknowledgement is only legible
	// next to what was actually said, and the transcript that sentence came from is
	// pruned, compacted and eventually gone.
	SteerAccountKey = "account"
)

// A judgement reuses the item vocabulary's provenance axis (ItemProvenanceKey) rather than
// growing a second one, for the reason an audit does. Nothing is run to reach it, so it is
// recorded asserted, and a reader can tell it apart from a verdict something was executed
// for without knowing a second vocabulary.
