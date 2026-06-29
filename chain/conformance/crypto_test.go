package conformance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/chain"
)

const cryptoDir = "testdata/crypto"

// cryptoRing builds the keyring a conforming verifier checks the suite against: the
// single published root public key. The "unknown key" vector is signed by a key kept
// out of this ring on purpose.
func cryptoRing(t *testing.T) *chain.RootKeyring {
	t.Helper()
	ring := chain.NewRootKeyring()
	if err := ring.Add(RootKeyID(), RootPublicKey()); err != nil {
		t.Fatal(err)
	}
	return ring
}

// verifyCrypto applies the verifier the vector's kind selects and returns the error
// (nil on accept).
func verifyCrypto(v CryptoVector, ring *chain.RootKeyring) error {
	switch v.Kind {
	case KindCheckpoint:
		_, err := chain.VerifyCheckpoint(v.Artifact, ring)
		return err
	case KindRun:
		_, err := chain.VerifyRun(v.Artifact, ring)
		return err
	case KindEventProof:
		_, err := chain.VerifyEventProof(v.Artifact, ring)
		return err
	case KindConsistency:
		_, _, err := chain.VerifyConsistencyProof(v.Artifact, ring)
		return err
	case KindGovernance:
		events, err := chain.VerifyRun(v.Artifact, ring)
		if err != nil {
			return err
		}
		return chain.VerifyGovernance(events)
	case KindGroundTruth:
		events, err := chain.VerifyRun(v.Artifact, ring)
		if err != nil {
			return err
		}
		return chain.VerifyGroundTruth(events)
	default:
		return nil
	}
}

// TestCryptoVectorsConform runs every L2/L3 vector through the real verifier its kind
// selects and asserts the declared verdict, and for a rejection the declared failure
// code. This is what makes the cryptographic tiers canon: the reference verifier and
// the published artifacts agree by construction.
func TestCryptoVectorsConform(t *testing.T) {
	ring := cryptoRing(t)
	for _, v := range GenerateCrypto() {
		t.Run(v.ID, func(t *testing.T) {
			err := verifyCrypto(v, ring)
			switch v.Expect {
			case Accept:
				if err != nil {
					t.Fatalf("expected accept, got error: %v", err)
				}
			case Reject:
				if err == nil {
					t.Fatal("expected reject, got accept")
				}
				if code := faultCode(err); code != v.FailureCode {
					t.Fatalf("wrong failure code: got %q want %q (err: %v)", code, v.FailureCode, err)
				}
			default:
				t.Fatalf("unknown verdict %q", v.Expect)
			}
		})
	}
}

type cryptoKey struct {
	KeyID        string `json:"key_id"`
	Algorithm    string `json:"algorithm"`
	PublicKeyHex string `json:"public_key_hex"`
}

type cryptoEntry struct {
	ID          string `json:"id"`
	Tier        string `json:"tier"`
	Kind        string `json:"kind"`
	Expect      string `json:"expect"`
	FailureCode string `json:"failure_code,omitempty"`
	Description string `json:"description"`
	Artifact    string `json:"artifact"`
}

type cryptoManifest struct {
	SuiteVersion string        `json:"suite_version"`
	Tiers        []string      `json:"tiers"`
	Keyring      []cryptoKey   `json:"keyring"`
	Vectors      []cryptoEntry `json:"vectors"`
}

// cryptoPath returns the deterministic relative file path for a vector's artifact.
func cryptoPath(v CryptoVector) string {
	dir := "valid"
	if v.Expect == Reject {
		dir = "invalid"
	}
	base := strings.ReplaceAll(v.ID, ".", "_")
	return filepath.ToSlash(filepath.Join(dir, base+".cbor"))
}

// buildCryptoManifest derives the manifest and the path-to-bytes file set.
func buildCryptoManifest() (cryptoManifest, map[string][]byte) {
	m := cryptoManifest{
		SuiteVersion: CryptoSuiteVersion,
		Tiers:        []string{"L2", "L3", "L4"},
		Keyring: []cryptoKey{{
			KeyID:        RootKeyID(),
			Algorithm:    "Ed25519",
			PublicKeyHex: hex.EncodeToString(RootPublicKey()),
		}},
	}
	files := map[string][]byte{}
	for _, v := range GenerateCrypto() {
		p := cryptoPath(v)
		files[p] = v.Artifact
		m.Vectors = append(m.Vectors, cryptoEntry{
			ID:          v.ID,
			Tier:        v.Tier,
			Kind:        string(v.Kind),
			Expect:      string(v.Expect),
			FailureCode: v.FailureCode,
			Description: v.Description,
			Artifact:    p,
		})
	}
	return m, files
}

func cryptoManifestBytes(m cryptoManifest) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic("conformance: marshal crypto manifest: " + err.Error())
	}
	return append(b, '\n')
}

// TestCryptoGoldenMatch checks the committed cryptographic fixtures and manifest
// match what the generator produces, so a drift is caught in review. Run with -update
// to regenerate after an intentional change.
func TestCryptoGoldenMatch(t *testing.T) {
	m, files := buildCryptoManifest()
	manBytes := cryptoManifestBytes(m)

	if *update {
		if err := os.RemoveAll(cryptoDir); err != nil {
			t.Fatal(err)
		}
		for p, b := range files {
			full := filepath.Join(cryptoDir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(cryptoDir, "manifest.json"), manBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d crypto vector files and manifest.json", len(files))
		return
	}

	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(cryptoDir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("missing crypto vector file %s (run: go test ./chain/conformance -update): %v", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("crypto vector file %s differs from the generator (run -update if intentional)", p)
		}
	}
	gotMan, err := os.ReadFile(filepath.Join(cryptoDir, "manifest.json"))
	if err != nil {
		t.Fatalf("missing crypto manifest.json (run -update): %v", err)
	}
	if !bytes.Equal(gotMan, manBytes) {
		t.Fatal("crypto manifest.json differs from the generator (run -update if intentional)")
	}
}

// TestCryptoNoFalseAccept asserts the soundness of the cryptographic tiers directly:
// no single-byte mutation of an accepting artifact ever verifies. Every signed or
// committed byte is critical, so a verifier that accepts a mutated artifact is
// unsound. This exercises the verifier, not just the fixed vectors.
func TestCryptoNoFalseAccept(t *testing.T) {
	ring := cryptoRing(t)
	var accepts []CryptoVector
	for _, v := range GenerateCrypto() {
		if v.Expect == Accept {
			accepts = append(accepts, v)
		}
	}
	if len(accepts) == 0 {
		t.Fatal("no accept vectors to mutate")
	}

	rapid.Check(t, func(rt *rapid.T) {
		v := accepts[rapid.IntRange(0, len(accepts)-1).Draw(rt, "vector")]
		idx := rapid.IntRange(0, len(v.Artifact)-1).Draw(rt, "byte")
		mask := byte(rapid.IntRange(1, 255).Draw(rt, "mask"))

		mutated := append([]byte{}, v.Artifact...)
		mutated[idx] ^= mask

		tampered := CryptoVector{Kind: v.Kind, Artifact: mutated}
		if err := verifyCrypto(tampered, ring); err == nil {
			rt.Fatalf("a one-byte mutation of %s at index %d verified", v.ID, idx)
		}
	})
}
