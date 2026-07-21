package chain

import (
	"crypto/ed25519"
	"testing"

	"pgregory.net/rapid"
)

func testKey(seedByte byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func sampleCheckpoint(t *testing.T) Checkpoint {
	t.Helper()
	tr, _ := buildTree(t, 5)
	root, err := tr.Root()
	if err != nil {
		t.Fatal(err)
	}
	return Checkpoint{Origin: "flynn://instance/abc", Size: tr.Size(), RootHash: root}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv, pub := testKey(0x11)
	signer, err := NewEd25519RootSigner("inst-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	cp := sampleCheckpoint(t)
	sc, err := signer.SignCheckpoint(cp)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst-1", pub); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyCheckpoint(sc.COSE, ring)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Origin != cp.Origin || got.Size != cp.Size || string(got.RootHash) != string(cp.RootHash) {
		t.Fatalf("checkpoint mismatch: %+v vs %+v", got, cp)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	priv, pub := testKey(0x22)
	signer, _ := NewEd25519RootSigner("inst-1", priv)
	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	_ = ring.Add("inst-1", pub)
	bad := append([]byte{}, sc.COSE...)
	bad[len(bad)-1] ^= 0xff
	if _, err := VerifyCheckpoint(bad, ring); err == nil {
		t.Fatal("verified a tampered signature")
	}
}

func TestVerifyRejectsUnknownKey(t *testing.T) {
	priv, _ := testKey(0x33)
	signer, _ := NewEd25519RootSigner("inst-1", priv)
	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCheckpoint(sc.COSE, NewRootKeyring()); err == nil {
		t.Fatal("verified against an empty keyring")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	priv, _ := testKey(0x44)
	_, otherPub := testKey(0x55)
	signer, _ := NewEd25519RootSigner("inst-1", priv)
	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	_ = ring.Add("inst-1", otherPub)
	if _, err := VerifyCheckpoint(sc.COSE, ring); err == nil {
		t.Fatal("verified under the wrong public key")
	}
}

func TestSignerRejectsBadInputs(t *testing.T) {
	priv, _ := testKey(0x66)
	if _, err := NewEd25519RootSigner("", priv); err == nil {
		t.Fatal("empty key id accepted")
	}
	if _, err := NewEd25519RootSigner("k", ed25519.PrivateKey{1, 2, 3}); err == nil {
		t.Fatal("malformed key accepted")
	}
}

func TestPropSignVerify(t *testing.T) {
	priv, pub := testKey(0x88)
	signer, _ := NewEd25519RootSigner("inst", priv)
	ring := NewRootKeyring()
	_ = ring.Add("inst", pub)
	rapid.Check(t, func(rt *rapid.T) {
		cp := Checkpoint{
			Origin:   rapid.StringMatching(`[a-z:/._-]{0,24}`).Draw(rt, "origin"),
			Size:     rapid.Uint64Range(0, 1<<40).Draw(rt, "size"),
			RootHash: rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "root"),
		}
		sc, err := signer.SignCheckpoint(cp)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		got, err := VerifyCheckpoint(sc.COSE, ring)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if got.Origin != cp.Origin || got.Size != cp.Size || string(got.RootHash) != string(cp.RootHash) {
			t.Fatalf("round-trip mismatch: %+v vs %+v", got, cp)
		}
	})
}

func FuzzVerifyCheckpointNoPanic(f *testing.F) {
	priv, pub := testKey(0x77)
	signer, _ := NewEd25519RootSigner("k", priv)
	sc, _ := signer.SignCheckpoint(Checkpoint{Origin: "o", Size: 1, RootHash: make([]byte, 32)})
	f.Add(sc.COSE)
	f.Add([]byte{})
	f.Add([]byte{0xff})
	ring := NewRootKeyring()
	_ = ring.Add("k", pub)
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = VerifyCheckpoint(data, ring)
	})
}
