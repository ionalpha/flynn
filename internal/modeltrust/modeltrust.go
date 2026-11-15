// Package modeltrust classifies how far a model run is trusted, which sets how
// strongly it must be contained.
//
// Before a downloaded model is parsed and run, what is known about it (how vetted
// its source is, whether its weights matched a pinned digest, whether the parsing
// runtime is past its known advisories) maps to an execution trust level. The
// containment gate in package sandbox uses that level, so an unvetted model runs
// only in a tier that can contain it, or is refused.
//
// The runtime's model-file parser is exposed to attack even for a benign file, so a
// run is untrusted unless every signal is good, and an unknown signal is untrusted.
package modeltrust

import (
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/sandbox"
)

// Signals are what is known about one model run, the inputs to its trust level.
type Signals struct {
	// Provenance is how far the model's source has been vetted (see catalog.Trust).
	Provenance catalog.Trust
	// IntegrityVerified reports that the weights matched a pinned digest, so they are
	// the vetted bytes.
	IntegrityVerified bool
	// RuntimeSafe reports that the parsing runtime passed the advisory gate, its
	// version at or above every known model-parser fix. The caller computes it with
	// package inference.
	RuntimeSafe bool
}

// Classify maps what is known about a model run to its execution trust level. A run
// is at most semi-trusted, and only with a blessed source, weights verified against
// a pinned digest and a runtime past its known advisories. Anything less is
// untrusted, because a discovered or unverified model or an unpatched parser can
// hand control to an attacker, so it gets the strongest boundary. A model run is
// never trusted: even a vetted model is foreign code parsed by a separate runtime.
func Classify(s Signals) sandbox.Trust {
	if s.Provenance == catalog.TrustBlessed && s.IntegrityVerified && s.RuntimeSafe {
		return sandbox.TrustSemi
	}
	return sandbox.TrustUntrusted
}

// RequiredContainment is the minimum isolation a model run with these signals may
// use, fed to the sandbox containment requirement.
func RequiredContainment(s Signals) sandbox.Containment {
	return sandbox.Required(Classify(s))
}
