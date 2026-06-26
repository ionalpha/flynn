package modeltrust

import (
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/sandbox"
)

// TestUnvettedRunRequiresHardwareTierAndIsRefusedBelow is the guarantee with teeth: a
// model run that is not fully vetted (blessed source, verified weights, safe runtime,
// all three) is classified untrusted, requires a hardware boundary, and is refused on
// the kernel-confined local tier. This composes the classifier with the existing
// containment gate, so it proves the end-to-end rule a clueless user relies on: a model
// from anywhere that is not fully vetted cannot run on anything weaker than a hardware
// boundary.
func TestUnvettedRunRequiresHardwareTierAndIsRefusedBelow(t *testing.T) {
	// The kernel-confined local tier is the strongest tier short of a hardware boundary
	// that exists today; an untrusted run must still be refused on it.
	kernelTier, err := sandbox.NewLocal(t.TempDir(), sandbox.WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}

	unvetted := []Signals{
		{}, // nothing known
		{Provenance: catalog.TrustDiscovered, IntegrityVerified: true, RuntimeSafe: true},
		{Provenance: catalog.TrustLocal, IntegrityVerified: true, RuntimeSafe: true},
		{Provenance: catalog.TrustBlessed, IntegrityVerified: false, RuntimeSafe: true},
		{Provenance: catalog.TrustBlessed, IntegrityVerified: true, RuntimeSafe: false},
	}
	for _, s := range unvetted {
		if got := Classify(s); got != sandbox.TrustUntrusted {
			t.Fatalf("unvetted run %+v classified %v, want untrusted", s, got)
		}
		if got := RequiredContainment(s); got != sandbox.ContainmentMicroVM {
			t.Fatalf("unvetted run %+v requires %v, want a hardware boundary", s, got)
		}
		if err := sandbox.Admit(kernelTier, Classify(s)); err == nil {
			t.Fatalf("unvetted run %+v was admitted on the kernel tier; it must be refused", s)
		}
	}

	// The only fully vetted case is semi-trusted, which the kernel tier may run.
	vetted := Signals{Provenance: catalog.TrustBlessed, IntegrityVerified: true, RuntimeSafe: true}
	if got := Classify(vetted); got != sandbox.TrustSemi {
		t.Fatalf("a fully vetted run classified %v, want semi-trusted", got)
	}
}
