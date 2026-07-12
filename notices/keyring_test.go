package notices_test

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
