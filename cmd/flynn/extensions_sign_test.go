package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeKeyFile writes priv as the JSON byte-array form loadDevSigner reads.
func writeKeyFile(t *testing.T, priv []byte) string {
	t.Helper()
	nums := make([]int, len(priv))
	for i, b := range priv {
		nums[i] = int(b)
	}
	raw, err := json.Marshal(nums)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestLoadDevSignerRoundTrips proves a 64-byte key file loads to a signer whose public half
// matches the key, and whose signatures verify.
func TestLoadDevSignerRoundTrips(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	signer, err := loadDevSigner(writeKeyFile(t, priv))
	if err != nil {
		t.Fatalf("loadDevSigner: %v", err)
	}
	if !bytes.Equal(signer.Public(), priv.Public().(ed25519.PublicKey)) {
		t.Fatal("loaded signer's public key does not match the key file")
	}
	sig, err := signer.Sign(context.Background(), []byte("hello"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !ed25519.Verify(signer.Public(), []byte("hello"), sig) {
		t.Fatal("signature from the loaded key does not verify")
	}
}

// TestLoadDevSignerRejectsBadKeys proves malformed key files are refused, not loaded into a
// signer that cannot sign.
func TestLoadDevSignerRejectsBadKeys(t *testing.T) {
	// Wrong length: a 32-byte seed is not the 64-byte private key.
	if _, err := loadDevSigner(writeKeyFile(t, bytes.Repeat([]byte{1}, ed25519.SeedSize))); err == nil {
		t.Fatal("expected a length error for a 32-byte key")
	}
	// Not JSON at all.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadDevSigner(bad); err == nil {
		t.Fatal("expected a parse error for a non-JSON key file")
	}
	// A byte value out of range.
	oor := filepath.Join(t.TempDir(), "oor.json")
	nums := make([]int, ed25519.PrivateKeySize)
	nums[0] = 999
	raw, _ := json.Marshal(nums)
	if err := os.WriteFile(oor, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadDevSigner(oor); err == nil {
		t.Fatal("expected an out-of-range error for a byte value above 255")
	}
	// A missing file.
	if _, err := loadDevSigner(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected an error for a missing key file")
	}
}
