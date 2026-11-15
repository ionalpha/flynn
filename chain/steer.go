package chain

// The steer vocabulary: an operator's redirect and the judgement of whether the
// run's account of finishing addressed it. InvariantAudited answers "is this still
// true"; this answers "was the interrupting operator answered". They are separate
// event types so a reader never gets one for the other.
//
// The status keeps only the outcome and is overwritten by the goal's next step.
// The event keeps the occasion: which redirect, what the run said, what the judge
// made of it, and when.
const (
	// SteerJudged rules on one redirect (SteerKey) against the run's account of
	// finishing, with the verdict in SteerAddressedKey. An addressed redirect also
	// carries SteerHowKey.
	SteerJudged = "steer.judged"
	SteerKey    = "steer"
	// SteerAddressedKey is true when the run's account addressed the redirect. False
	// refuses the run's completion and is recorded, not left to the status, because
	// a run that ignored its operator has the least reliable account of itself.
	SteerAddressedKey = "addressed"
	// SteerHowKey quotes the part of the account the judge accepted. An addressed
	// verdict with nothing quoted is refused, so every accepted acknowledgement
	// carries it.
	SteerHowKey = "how"
	// SteerAccountKey holds the run's statement verbatim. A refusal is legible only
	// next to what was said, and the transcript is eventually pruned.
	SteerAccountKey = "account"
)
