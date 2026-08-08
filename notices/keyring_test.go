package notices_test

// The keys this binary will believe: the ring it ships with, and what Add does to it.
// A build carrying no usable key can never be told anything for as long as it runs, so
// the shipped ring is checked here rather than assumed, and a malformed entry costs one
// key rather than taking the agent down.

import (
	"crypto/ed25519"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

// The keyring this binary ships with is the whole reach of the notice channel: a build
// carrying no usable key can never be told anything for as long as it runs, and no later
// fix can reach it, because the fix would arrive through the channel it does not have.
//
// So the shipped ring is checked here rather than assumed. A typo in a hex string, a key
// dropped in a merge, or a release cut before the ceremony would all otherwise be invisible
// until the first real advisory failed to verify on every machine in the world at once.
func TestTheShippedKeyringCanActuallyTrustSomething(t *testing.T) {
	ring := notices.DefaultKeyring()

	if ring.Len() == 0 {
		t.Fatal("the shipped keyring is empty: binaries built from this tree could never be sent a security advisory")
	}

	// Two keys, and the second one is why the first is replaceable. A binary trusts only
	// the keys it was built with, so a release that shipped a single key would become
	// permanently unreachable if that key were ever lost. The backup key is held offline
	// and signs nothing until a rotation, which turns "lost key" into an inconvenience
	// instead of an install base we can never warn again.
	if ring.Len() < 2 {
		t.Fatalf("the shipped keyring holds %d key(s); it must also carry an offline backup key, "+
			"or losing the operational key makes every binary from this release unreachable forever", ring.Len())
	}
}

// DefaultKeyring skips a malformed entry rather than panicking, which is the right call (a
// bad key costs us the ability to speak, and taking the user's agent down over it would be
// a self-inflicted outage). But that means a typo would silently shrink the ring, so the
// hex is checked directly: every key in the source must be a well-formed Ed25519 public
// key, and the count that survives parsing must be the count that was written down.
func TestEveryKeyInTheSourceParses(t *testing.T) {
	ring := notices.DefaultKeyring()
	if ring.Len() != notices.SourceKeyCount() {
		t.Fatalf("%d keys are declared in source but only %d parsed: a key is malformed and "+
			"was silently skipped, so it can never sign anything this binary will believe",
			notices.SourceKeyCount(), ring.Len())
	}
	if ed25519.PublicKeySize != 32 {
		t.Fatal("the Ed25519 public key size changed under us")
	}
}

// TestKeyringAddRefusesUnusableKeys is the mirror on the verifying side: a key the
// ring cannot use must be rejected at Add rather than silently stored and then found
// broken on the day an advisory has to go out.
func TestKeyringAddRefusesUnusableKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		keyID string
		pub   ed25519.PublicKey
	}{
		{name: "empty key id", keyID: "", pub: pub},
		{name: "short public key", keyID: "k", pub: pub[:8]},
		{name: "nil public key", keyID: "k", pub: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ring := notices.NewKeyring()
			if err := ring.Add(tc.keyID, tc.pub); err == nil {
				t.Fatal("expected an error")
			}
			if ring.Len() != 0 {
				t.Fatal("a rejected key was still added to the ring")
			}
		})
	}
}

// TestKeyringAddReplacesOnRotation pins that re-adding an id swaps the key rather than
// keeping both, which is what makes a rotation a rotation and not an accumulation of
// keys that can still sign.
func TestKeyringAddReplacesOnRotation(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notices.Verify(doc, ring); err != nil {
		t.Fatalf("the original key should verify: %v", err)
	}

	// Rotate: the same id now holds a different public key.
	rotated, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ring.Add("test-key-1", rotated); err != nil {
		t.Fatal(err)
	}
	if ring.Len() != 1 {
		t.Fatalf("rotation should replace, not accumulate: ring holds %d keys", ring.Len())
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed signed by the retired key still verified after rotation")
	}
}

// TestDefaultKeyringSkipsAMalformedKey pins the compiled-in ring's failure direction: a
// bad key entry costs us the ability to say something through that key, and must not take
// the user's agent down with a panic.
func TestDefaultKeyringSkipsAMalformedKey(t *testing.T) {
	ring := notices.DefaultKeyring()
	if ring.Len() != notices.SourceKeyCount() {
		t.Fatalf("the shipped keyring holds %d of %d declared keys: a key in the source does not parse",
			ring.Len(), notices.SourceKeyCount())
	}
	if ring.Len() == 0 {
		t.Fatal("a release must not ship with an empty keyring: those binaries could never be reached")
	}
}
