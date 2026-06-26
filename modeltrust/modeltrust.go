// Package modeltrust classifies how far a model run is trusted, which sets how
// strongly it must be contained.
//
// It is the first stage of the safe path for running a downloaded model: before a
// model is parsed and executed, what is known about it (how vetted its source is,
// whether its weights were verified against a pinned digest, whether the runtime that
// will parse them is past its known advisories) is mapped to an execution trust level.
// That level drives the containment gate in package sandbox, so an unvetted model is
// admitted only to a tier strong enough to contain it, or refused.
//
// The policy is strict on purpose: the runtime's model-file parser is the attack
// surface even for a benign file, so a run is treated as untrusted unless every signal
// is good. The classification never silently trusts; an unknown is an untrusted.
package modeltrust

import (
	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/sandbox"
)

// Signals are what is known about a specific model run, the inputs that set how far it
// is trusted.
type Signals struct {
	// Provenance is how far the model's source has been vetted (see catalog.Trust).
	Provenance catalog.Trust
	// IntegrityVerified reports that the weights matched a pinned digest, so the bytes
	// are the bytes that were vetted.
	IntegrityVerified bool
	// RuntimeSafe reports that the runtime that will parse the weights passed the
	// advisory gate (its version is at or above every known model-parser fix). The
	// caller computes this with package inference and passes the result.
	RuntimeSafe bool
}

// Classify maps what is known about a model run to its execution trust level. A run is
// trusted only as far as semi-trusted, and only when its source is blessed, its weights
// were verified against a pinned digest, and the runtime that parses them is past its
// known advisories. Anything less is untrusted: a discovered or unverified model, or a
// runtime with an unpatched parser, can hand control to an attacker through the parser,
// so it gets the strongest boundary. A model run is never classified trusted: even a
// fully vetted model is foreign code parsed by a separate runtime, not the agent's own
// vetted tools.
func Classify(s Signals) sandbox.Trust {
	if s.Provenance == catalog.TrustBlessed && s.IntegrityVerified && s.RuntimeSafe {
		return sandbox.TrustSemi
	}
	return sandbox.TrustUntrusted
}

// RequiredContainment is the minimum isolation a model run with these signals may run
// under, the classification fed straight into the sandbox containment requirement.
func RequiredContainment(s Signals) sandbox.Containment {
	return sandbox.Required(Classify(s))
}
