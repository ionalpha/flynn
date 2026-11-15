package chain

// The invariant vocabulary: an audit of one term of the run, ruling on whether it
// still holds. ItemVerified answers "is this work done"; this answers "is this
// still true". They are separate event types so a reader never gets one for the
// other. An item's verification is spent once; a term's audit is repeated after
// every step.
//
// A term is what a run is held to unobserved, so proof that it was checked must
// outlive the run. A status field is the run's own account of itself; this is the
// log carrying the exit code and output hash of the check.
const (
	// InvariantAudited is one audited term, named by InvariantKey with the verdict
	// in InvariantHeldKey. A broken term also carries InvariantDetailKey.
	InvariantAudited = "invariant.audited"
	InvariantKey     = "invariant"
	// InvariantHeldKey is true when the term still holds. A breach is recorded here,
	// not left to the status, because a run that broke its terms has the least
	// reliable account of itself.
	InvariantHeldKey = "held"
	// InvariantDetailKey holds what the audit observed about a broken term. It is
	// quoted to whoever receives the stopped goal.
	InvariantDetailKey = "detail"
	// InvariantCitedKey holds the part of the run's record an audit that ran no
	// command read to reach its verdict. A verdict with nothing cited is refused, so
	// every asserted audit carries it. An executed audit does not: the command line
	// and exit code are the citation.
	InvariantCitedKey = "cited"
)
