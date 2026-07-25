//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hasAllowAceFor reports whether path's access list carries an allow entry for sid, comparing
// the trustee SID by value rather than by its textual form. The SDDL rendering of a
// well-known SID uses an alias (ALL APPLICATION PACKAGES prints as "AC", not "S-1-15-2-1"),
// so a string match on the descriptor is not reliable across Windows builds; walking the ACEs
// and comparing SIDs is.
func hasAllowAceFor(t *testing.T, path string, sid *windows.SID) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security info for %s: %v", path, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read DACL for %s: %v", path, err)
	}
	if acl == nil {
		return false
	}
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID.Equals(sid) {
			return true
		}
	}
	return false
}

// TestGrantSealedKeyReadableAddsAllPackagesACE proves the grant lands an ALL APPLICATION
// PACKAGES allow entry on the sealed key file and the directories above it, which is what
// lets a confined signer's AppContainer open the key the host named for it. Without this the
// read is denied and the mint never reaches signing.
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

	// The key file and both parent directories must carry the entry: the file so the key can
	// be read, the directories so the container can traverse down to it.
	for _, path := range []string{key, signers, dir} {
		if !hasAllowAceFor(t, path, aap) {
			t.Fatalf("%s is missing an ALL APPLICATION PACKAGES allow entry", path)
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
	for i := range 3 {
		if err := GrantSealedKeyReadable(key); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
}
