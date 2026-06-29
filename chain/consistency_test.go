package chain

import (
	"testing"

	"pgregory.net/rapid"
)

// growingLog builds a Merkle log of size2 events and returns a consistency proof
// between the signed checkpoints at size1 and size2, plus a ring holding the signing
// key. It is the producer side of a consistency proof: one growing log, two signed
// heads, and the path that connects them.
func growingLog(t *testing.T, size1, size2 int) (*ConsistencyProof, *RootKeyring) {
	t.Helper()
	priv, pub := testKey(0x21)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	tree := NewTree()
	sign := func(n int) []byte {
		root, rerr := tree.Root()
		if rerr != nil {
			t.Fatal(rerr)
		}
		sc, serr := signer.SignCheckpoint(Checkpoint{Origin: "flynn://run/grow", Size: uint64(n), RootHash: root})
		if serr != nil {
			t.Fatal(serr)
		}
		return sc.COSE
	}

	var before []byte
	for i := range size2 {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		cb, cerr := CanonicalBytes(e)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := tree.Append(cb); err != nil {
			t.Fatal(err)
		}
		if i+1 == size1 {
			before = sign(size1)
		}
	}
	after := sign(size2)
	path, err := tree.ConsistencyProof(uint64(size1))
	if err != nil {
		t.Fatal(err)
	}

	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	return &ConsistencyProof{Before: before, After: after, Proof: path}, ring
}

func TestVerifyConsistencyProofRoundTrip(t *testing.T) {
	p, ring := growingLog(t, 2, 5)
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	before, after, err := VerifyConsistencyProof(raw, ring)
	if err != nil {
		t.Fatalf("a valid consistency proof failed: %v", err)
	}
	if before.Size != 2 || after.Size != 5 {
		t.Fatalf("recovered wrong sizes: before=%d after=%d", before.Size, after.Size)
	}
}

func TestVerifyConsistencyProofRejectsUnknownKey(t *testing.T) {
	p, _ := growingLog(t, 2, 5)
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyConsistencyProof(raw, NewRootKeyring()); err == nil {
		t.Fatal("verified a consistency proof against an empty keyring")
	}
}

// TestVerifyConsistencyProofRejectsForgedPath confirms a proof whose path does not
// connect the two signed roots is rejected, so the artifact cannot claim a fork or a
// rewrite is an honest append.
func TestVerifyConsistencyProofRejectsForgedPath(t *testing.T) {
	p, ring := growingLog(t, 2, 5)
	if len(p.Proof) > 0 {
		p.Proof[0] = append([]byte{}, p.Proof[0]...)
		p.Proof[0][0] ^= 0xff
	}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyConsistencyProof(raw, ring); err == nil {
		t.Fatal("verified a consistency proof with a forged path")
	}
}

// TestConsistencyProofNoFalseAccept asserts no single-byte mutation of a valid
// consistency proof ever verifies: both checkpoints are signed and the path is bound
// to their roots, so every byte is critical.
func TestConsistencyProofNoFalseAccept(t *testing.T) {
	p, ring := growingLog(t, 2, 5)
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		idx := rapid.IntRange(0, len(raw)-1).Draw(rt, "byte")
		mask := byte(rapid.IntRange(1, 255).Draw(rt, "mask"))
		mutated := append([]byte{}, raw...)
		mutated[idx] ^= mask
		if _, _, err := VerifyConsistencyProof(mutated, ring); err == nil {
			rt.Fatalf("a one-byte mutation at index %d verified", idx)
		}
	})
}
