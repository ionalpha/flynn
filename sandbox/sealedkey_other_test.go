//go:build !windows

package sandbox

import "testing"

// TestGrantSealedKeyReadableNoopOffWindows proves the grant is a no-op off Windows: there is
// no AppContainer and no package SID, so it must return nil without touching the path (which
// need not even exist).
func TestGrantSealedKeyReadableNoopOffWindows(t *testing.T) {
	if err := GrantSealedKeyReadable("/does/not/exist/solana-signer.key"); err != nil {
		t.Fatalf("expected a no-op returning nil off Windows, got %v", err)
	}
}
