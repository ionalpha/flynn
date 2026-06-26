package inference

import (
	"testing"

	"pgregory.net/rapid"
)

// TestProp_VersionOrderAndGate covers the two invariants the gate rests on across
// generated versions: Less is a strict total order (so version comparison is
// well-defined), and the gate's verdict is exactly its exposure (refused if and only if
// the version predates a fix for its runtime), so the gate can never disagree with the
// advisory data it is built from.
func TestProp_VersionOrderAndGate(t *testing.T) {
	verGen := rapid.SliceOfN(rapid.IntRange(0, 100000), 0, 5)

	rapid.Check(t, func(rt *rapid.T) {
		a := Version(verGen.Draw(rt, "a"))
		b := Version(verGen.Draw(rt, "b"))

		// Irreflexive and asymmetric: a strict order never has both a<b and b<a, and
		// never a<a.
		if a.Less(a) {
			rt.Fatalf("Less is reflexive on %v", a)
		}
		if a.Less(b) && b.Less(a) {
			rt.Fatalf("Less is symmetric on %v and %v", a, b)
		}

		// The gate refuses exactly when there is an exposure, for every runtime.
		for _, runtime := range []string{"llama.cpp", "ollama", "unknown"} {
			exposed := len(Exposure(runtime, a, Advisories())) > 0
			refused := SafeToRun(runtime, a) != nil
			if exposed != refused {
				rt.Fatalf("%s %v: exposed=%v but refused=%v", runtime, a, exposed, refused)
			}
		}
	})
}
