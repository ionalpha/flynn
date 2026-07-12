package inference

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// A floor arriving from the signed feed raises the gate, and a refusal names the advisory
// the running build was never compiled knowing about.
func TestRaiseTightensTheGate(t *testing.T) {
	t.Cleanup(resetOverlay)
	resetOverlay()

	// b8146 is the compiled-in llama.cpp floor, so this version passes today.
	if err := SafeToRun("llama.cpp", Version{8200}); err != nil {
		t.Fatalf("a version above the built-in floor was refused: %v", err)
	}

	Raise("llama.cpp", Version{8300}, "FLYNN-RT-2026-0007")

	err := SafeToRun("llama.cpp", Version{8200})
	if err == nil {
		t.Fatal("a version below the raised floor was still allowed to parse a model file")
	}
	if !strings.Contains(err.Error(), "FLYNN-RT-2026-0007") {
		t.Fatalf("the refusal did not name the advisory the floor came from: %v", err)
	}
	if err := SafeToRun("llama.cpp", Version{8300}); err != nil {
		t.Fatalf("a version at the raised floor was refused: %v", err)
	}
}

// The law. This is the property the whole network-delivered-policy design rests on: a feed
// can tighten the gate and can never loosen it. If this can be broken, an attacker with the
// signing key gets a path to running a vulnerable parser, and the channel becomes a
// liability instead of a defence.
func TestNoFeedCanEverLowerAFloor(t *testing.T) {
	t.Cleanup(resetOverlay)

	rapid.Check(t, func(rt *rapid.T) {
		resetOverlay()

		built, ok := builtinFloor("llama.cpp")
		if !ok {
			rt.Fatal("llama.cpp has no compiled-in floor to defend")
		}

		// An attacker who holds the origin and the key sends whatever they like, in any
		// order, as many times as they like.
		n := rapid.IntRange(1, 8).Draw(rt, "attempts")
		for range n {
			Raise("llama.cpp",
				Version{rapid.IntRange(0, 20000).Draw(rt, "claimed")},
				rapid.StringN(0, 12, -1).Draw(rt, "advisory"))
		}

		floor, ok, _ := FloorFor("llama.cpp")
		if !ok {
			rt.Fatal("the floor disappeared entirely, which would gate nothing")
		}
		if floor.Less(built) {
			rt.Fatalf("a feed lowered the floor from %s to %s", built, floor)
		}
		// And the gate still refuses everything the compiled-in floor refused.
		if err := SafeToRun("llama.cpp", Version{0}); err == nil {
			rt.Fatal("after applying feed floors, an ancient runtime was allowed to run")
		}
	})
}

// A floor lower than the one in force is a no-op, not an error. It must not be usable to
// make the notice path fail: a caller that aborted on it would be a caller an attacker
// could use to skip the rest of a feed, including the advisory that names their exploit.
func TestALowerFloorIsIgnoredRatherThanFailing(t *testing.T) {
	t.Cleanup(resetOverlay)
	resetOverlay()

	Raise("llama.cpp", Version{9000}, "FLYNN-RT-A")
	Raise("llama.cpp", Version{1}, "FLYNN-RT-DOWNGRADE") // the attack
	Raise("llama.cpp", Version{9500}, "FLYNN-RT-B")

	floor, _, advisory := FloorFor("llama.cpp")
	if floor.Less(Version{9500}) {
		t.Fatalf("the floor ended at %s; the downgrade attempt was not ignored", floor)
	}
	if advisory != "FLYNN-RT-B" {
		t.Fatalf("the floor is attributed to %q, not the advisory that raised it", advisory)
	}
}

// A feed cannot invent a runtime, and cannot gate one Flynn does not drive.
func TestAnUnknownRuntimeIsIgnored(t *testing.T) {
	t.Cleanup(resetOverlay)
	resetOverlay()

	Raise("definitely-not-a-runtime", Version{9999}, "x")
	if _, ok, _ := FloorFor("definitely-not-a-runtime"); ok {
		t.Fatal("a feed invented a runtime and gated it")
	}
	// And a runtime Flynn does drive is unaffected by the attempt.
	if err := SafeToRun("llama.cpp", Version{8146}); err != nil {
		t.Fatalf("a bogus floor disturbed a real runtime's gate: %v", err)
	}
}

// An empty version gates nothing and must not wipe a floor that is in force.
func TestAnEmptyFloorDoesNotClearTheGate(t *testing.T) {
	t.Cleanup(resetOverlay)
	resetOverlay()

	Raise("llama.cpp", Version{9000}, "FLYNN-RT-A")
	Raise("llama.cpp", nil, "")
	Raise("llama.cpp", Version{}, "")

	if floor, _, _ := FloorFor("llama.cpp"); floor.Less(Version{9000}) {
		t.Fatalf("an empty floor cleared the gate down to %s", floor)
	}
}
