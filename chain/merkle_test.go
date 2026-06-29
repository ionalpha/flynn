package chain

import (
	"testing"

	"pgregory.net/rapid"
)

// buildTree appends n events (with distinct Seq) and returns the tree and the leaf
// hashes, so a test can verify proofs against known leaves.
func buildTree(t *testing.T, n int) (*Tree, [][]byte) {
	t.Helper()
	tr := NewTree()
	leaves := make([][]byte, n)
	for i := range n {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		cb, err := CanonicalBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(cb); err != nil {
			t.Fatal(err)
		}
		lh, err := LeafHash(cb)
		if err != nil {
			t.Fatal(err)
		}
		leaves[i] = lh
	}
	return tr, leaves
}

func flip(b []byte) []byte {
	out := append([]byte{}, b...)
	if len(out) > 0 {
		out[0] ^= 0xff
	}
	return out
}

func TestTreeSingleLeafRootIsLeaf(t *testing.T) {
	tr, leaves := buildTree(t, 1)
	root, err := tr.Root()
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != string(leaves[0]) {
		t.Fatal("single-leaf tree root must equal the leaf hash")
	}
	pf, err := tr.InclusionProof(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInclusion(0, 1, leaves[0], root, pf); err != nil {
		t.Fatalf("single-leaf inclusion rejected: %v", err)
	}
}

// TestPropInclusion asserts that across many tree sizes and indices, a generated
// inclusion proof verifies, and that tampering with the root, the leaf, or any
// proof node makes verification fail. The no-false-accept property is the point.
func TestPropInclusion(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 48).Draw(rt, "n")
		tr, leaves := buildTree(t, n)
		root, err := tr.Root()
		if err != nil {
			t.Fatal(err)
		}
		idx := rapid.IntRange(0, n-1).Draw(rt, "idx")
		pf, err := tr.InclusionProof(uint64(idx))
		if err != nil {
			t.Fatalf("inclusion proof: %v", err)
		}
		if err := VerifyInclusion(uint64(idx), uint64(n), leaves[idx], root, pf); err != nil {
			t.Fatalf("valid inclusion proof rejected (n=%d idx=%d): %v", n, idx, err)
		}
		if err := VerifyInclusion(uint64(idx), uint64(n), leaves[idx], flip(root), pf); err == nil {
			t.Fatal("inclusion verified against a tampered root")
		}
		if err := VerifyInclusion(uint64(idx), uint64(n), flip(leaves[idx]), root, pf); err == nil {
			t.Fatal("inclusion verified for a tampered leaf")
		}
		if len(pf) > 0 {
			j := rapid.IntRange(0, len(pf)-1).Draw(rt, "j")
			bad := make([][]byte, len(pf))
			copy(bad, pf)
			bad[j] = flip(pf[j])
			if err := VerifyInclusion(uint64(idx), uint64(n), leaves[idx], root, bad); err == nil {
				t.Fatal("inclusion verified with a tampered proof node")
			}
		}
	})
}

// TestPropConsistency asserts that a prefix tree's root is provably consistent with
// the larger tree, and that a tampered earlier root is rejected. Because the leaves
// are deterministic, the size-m tree is a genuine prefix of the size-n tree.
func TestPropConsistency(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 48).Draw(rt, "n")
		m := rapid.IntRange(1, n).Draw(rt, "m")
		trN, _ := buildTree(t, n)
		trM, _ := buildTree(t, m)
		rootN, err := trN.Root()
		if err != nil {
			t.Fatal(err)
		}
		rootM, err := trM.Root()
		if err != nil {
			t.Fatal(err)
		}
		pf, err := trN.ConsistencyProof(uint64(m))
		if err != nil {
			t.Fatalf("consistency proof: %v", err)
		}
		if err := VerifyConsistency(uint64(m), uint64(n), rootM, rootN, pf); err != nil {
			t.Fatalf("valid consistency proof rejected (m=%d n=%d): %v", m, n, err)
		}
		if m != n {
			if err := VerifyConsistency(uint64(m), uint64(n), flip(rootM), rootN, pf); err == nil {
				t.Fatal("consistency verified against a tampered earlier root")
			}
		}
	})
}

// FuzzVerifyProofNoPanic feeds arbitrary inputs to the proof verifiers, which are
// attacker-facing. They must never panic, only return an error.
func FuzzVerifyProofNoPanic(f *testing.F) {
	f.Add(uint64(0), uint64(1), []byte("leaf"), []byte("root"), []byte("proofnodesproofnodesproofnodes12"))
	f.Fuzz(func(_ *testing.T, index, size uint64, leaf, root, flat []byte) {
		var pf [][]byte
		for i := 0; i+32 <= len(flat) && len(pf) < 64; i += 32 {
			pf = append(pf, flat[i:i+32])
		}
		_ = VerifyInclusion(index, size, leaf, root, pf)
		_ = VerifyConsistency(0, size, leaf, root, pf)
	})
}
