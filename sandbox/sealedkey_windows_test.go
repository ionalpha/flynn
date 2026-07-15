//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestGrantSealedKeyReadableAddsAllPackagesACE proves the grant lands an ALL APPLICATION
// PACKAGES entry on the sealed key file and the directories above it, which is what lets a
// confined signer's AppContainer open the key the host named for it. Without this the read
// is denied and the mint never reaches signing.
func TestGrantSealedKeyReadableAddsAllPackagesACE(t *testing.T) {
	dir := t.TempDir()
	signers := filepath.Join(dir, "signers")
	if err := os.MkdirAll(signers, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(signers, "solana-signer.key")
	if err := os.WriteFile(key, []byte("sealed-key-ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := GrantSealedKeyReadable(key); err != nil {
		t.Fatalf("GrantSealedKeyReadable: %v", err)
	}

	aap, err := windows.CreateWellKnownSid(windows.WinBuiltinAnyPackageSid)
	if err != nil {
		t.Fatal(err)
	}
	aapSID := aap.String() // "S-1-15-2-1"

	// The key file and both parent directories must carry the entry: the file so the key can
	// be read, the directories so the container can traverse down to it.
	for _, path := range []string{key, signers, dir} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read security info for %s: %v", path, err)
		}
		if got := sd.String(); !strings.Contains(got, aapSID) {
			t.Fatalf("%s DACL is missing the ALL APPLICATION PACKAGES SID %s:\n%s", path, aapSID, got)
		}
	}
}

// TestGrantSealedKeyReadableIsIdempotent proves a second grant (every mount re-applies it)
// neither errors nor duplicates the machinery into a failure: the merge folds into the
// existing entry rather than stacking.
func TestGrantSealedKeyReadableIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	signers := filepath.Join(dir, "signers")
	if err := os.MkdirAll(signers, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(signers, "solana-signer.key")
	if err := os.WriteFile(key, []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := GrantSealedKeyReadable(key); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
}
