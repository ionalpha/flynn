package notices_test

// The publishing side's guards. A signer built on an unusable key would produce a feed
// nobody can attribute and so nobody can revoke, or signatures no client can check, and
// either is discovered on the day an advisory has to go out. NewSigner refuses both up
// front.

import (
	"crypto/ed25519"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

// TestNewSignerRefusesUnusableKeys covers the publishing side's guards. An empty key
// id produces a feed nobody can attribute, and so nobody can revoke; a malformed
// private key would produce signatures no client can check.
func TestNewSignerRefusesUnusableKeys(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	good := ed25519.NewKeyFromSeed(seed)

	tests := []struct {
		name  string
		keyID string
		priv  ed25519.PrivateKey
	}{
		{name: "empty key id", keyID: "", priv: good},
		{name: "short private key", keyID: "k", priv: good[:16]},
		{name: "nil private key", keyID: "k", priv: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := notices.NewSigner(tc.keyID, tc.priv)
			if err == nil {
				t.Fatal("expected an error")
			}
			if s != nil {
				t.Fatal("a rejected key still produced a signer")
			}
		})
	}

	s, err := notices.NewSigner("flynn-notices-1", good)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.KeyID(); got != "flynn-notices-1" {
		t.Fatalf("KeyID = %q, want %q", got, "flynn-notices-1")
	}
}
