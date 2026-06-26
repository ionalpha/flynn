package modeltrust

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/sandbox"
)

// TestProp_ClassificationAndRefusal pins the classifier's rule and its consequence
// across every combination of signals: a run is semi-trusted exactly when its source is
// blessed and its weights are verified and its runtime is safe, and untrusted otherwise;
// and an untrusted run always requires a hardware boundary and is always refused on a
// tier weaker than that. There is no combination of signals that lets an unvetted model
// onto a tier that cannot contain it.
func TestProp_ClassificationAndRefusal(t *testing.T) {
	provGen := rapid.SampledFrom([]catalog.Trust{
		catalog.TrustBlessed, catalog.TrustDiscovered, catalog.TrustLocal, catalog.Trust(""),
	})
	weakTier, err := sandbox.NewLocal(t.TempDir(), sandbox.WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}

	rapid.Check(t, func(rt *rapid.T) {
		s := Signals{
			Provenance:        provGen.Draw(rt, "provenance"),
			IntegrityVerified: rapid.Bool().Draw(rt, "verified"),
			RuntimeSafe:       rapid.Bool().Draw(rt, "runtimeSafe"),
		}
		fullyVetted := s.Provenance == catalog.TrustBlessed && s.IntegrityVerified && s.RuntimeSafe

		got := Classify(s)
		want := sandbox.TrustUntrusted
		if fullyVetted {
			want = sandbox.TrustSemi
		}
		if got != want {
			rt.Fatalf("Classify(%+v) = %v, want %v", s, got, want)
		}

		// The safety consequence: an untrusted run needs a hardware boundary and is
		// refused on the strongest non-hardware tier there is.
		if got == sandbox.TrustUntrusted {
			if RequiredContainment(s) != sandbox.ContainmentMicroVM {
				rt.Fatalf("untrusted run %+v does not require a hardware boundary", s)
			}
			if sandbox.Admit(weakTier, got) == nil {
				rt.Fatalf("untrusted run %+v admitted on a non-hardware tier", s)
			}
		}
	})
}
