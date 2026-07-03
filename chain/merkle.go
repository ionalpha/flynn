package chain

import (
	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"

	"github.com/ionalpha/flynn/fault"
)

// merkleHasher is the RFC 6962 tree hasher (SHA-256) the log is defined over. It
// distinguishes leaf hashes from internal-node hashes by prefix, which prevents a
// second-preimage attack that swaps a leaf for an internal node.
var merkleHasher = rfc6962.DefaultHasher

// Merkle failure codes, matching the published Provetrail conformance registry.
const (
	CodeMissingNode        = "merkle.missing_node"
	CodeInclusionInvalid   = "merkle.inclusion_invalid"
	CodeConsistencyInvalid = "merkle.consistency_invalid"
)

// LeafHash returns the RFC 6962 leaf hash of an event's canonical bytes. The bytes
// are first wrapped by LeafInput, so the hash commits to the domain-separated,
// length-framed preimage rather than the raw canonical bytes.
func LeafHash(canonical []byte) ([]byte, error) {
	in, err := LeafInput(canonical)
	if err != nil {
		return nil, err
	}
	return merkleHasher.HashLeaf(in), nil
}

type nodeKey struct {
	level uint
	index uint64
}

// Tree is an append-only RFC 6962 Merkle log over event leaf hashes. It computes
// the signed-tree-head root and produces inclusion and consistency proofs. The
// hashing and proof verification are delegated to the transparency-dev/merkle
// library; the perfect-subtree node hashes a proof needs are kept in a nodeStore,
// so the same tree can hold them in memory or page them into tiles. A Tree is not
// safe for concurrent mutation.
type Tree struct {
	factory *compact.RangeFactory
	rang    *compact.Range
	store   nodeStore
	leaves  uint64
}

// NewTree returns an empty Merkle log whose nodes are held in memory.
func NewTree() *Tree { return newTreeWithStore(newMemNodeStore()) }

// newTreeWithStore returns an empty Merkle log backed by store. It is the shared
// constructor: NewTree pairs it with the in-memory map, and a durable log pairs it
// with a tiled, storage-backed store so proof nodes need not all be resident.
func newTreeWithStore(store nodeStore) *Tree {
	factory := &compact.RangeFactory{Hash: merkleHasher.HashChildren}
	return &Tree{
		factory: factory,
		rang:    factory.NewEmptyRange(0),
		store:   store,
	}
}

// Append adds an event's canonical bytes as the next leaf. It records the leaf and
// every internal node the append completes into the store, so later proofs can be
// assembled without recomputing the tree.
func (t *Tree) Append(canonical []byte) error {
	leaf, err := LeafHash(canonical)
	if err != nil {
		return err
	}
	if err := t.store.putNode(0, t.leaves, leaf); err != nil {
		return err
	}
	var putErr error
	if err := t.rang.Append(leaf, func(id compact.NodeID, hash []byte) {
		if putErr == nil {
			putErr = t.store.putNode(id.Level, id.Index, hash)
		}
	}); err != nil {
		return fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	if putErr != nil {
		return putErr
	}
	t.leaves++
	return nil
}

// Size is the number of leaves appended so far.
func (t *Tree) Size() uint64 { return t.leaves }

// Root returns the current Merkle tree head. An empty tree returns the hasher's
// empty root.
func (t *Tree) Root() ([]byte, error) {
	root, err := t.rang.GetRootHash(nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return root, nil
}

// InclusionProof returns the audit path proving the leaf at index is included in
// the tree at its current size.
func (t *Tree) InclusionProof(index uint64) ([][]byte, error) {
	nodes, err := proof.Inclusion(index, t.leaves)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeInclusionInvalid, err)
	}
	return t.assemble(nodes)
}

// ConsistencyProof returns the proof that the tree at the earlier size is a prefix
// of the tree at its current size, which is what makes the log verifiably
// append-only.
func (t *Tree) ConsistencyProof(size uint64) ([][]byte, error) {
	nodes, err := proof.Consistency(size, t.leaves)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeConsistencyInvalid, err)
	}
	return t.assemble(nodes)
}

// assemble looks up the hashes for a proof's node IDs and rehashes them into the
// final proof, collapsing the single ephemeral node the library may require.
func (t *Tree) assemble(nodes proof.Nodes) ([][]byte, error) {
	return assembleProof(t.store, nodes)
}

// assembleProof reads a proof's node IDs from a store, so a sealed run's retained
// nodes can produce proofs without a live Tree.
func assembleProof(store nodeStore, nodes proof.Nodes) ([][]byte, error) {
	hashes := make([][]byte, len(nodes.IDs))
	for i, id := range nodes.IDs {
		h, ok, err := store.getNode(id.Level, id.Index)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fault.New(fault.Terminal, CodeMissingNode, "chain: proof references a node not in the tree")
		}
		hashes[i] = h
	}
	return nodes.Rehash(hashes, merkleHasher.HashChildren)
}

// cloneStore copies the node store for a seal-time snapshot, so the sealed run's
// proof material is decoupled from further appends to the live tree.
func (t *Tree) cloneStore() nodeStore {
	return t.store.clone()
}

// VerifyInclusion reports whether pf proves that leafHash is the leaf at index in a
// tree of the given size whose head is root. It is the third-party check: a verifier
// needs only the leaf hash, the signed root, and the proof, not the whole log.
func VerifyInclusion(index, size uint64, leafHash, root []byte, pf [][]byte) error {
	if err := proof.VerifyInclusion(merkleHasher, index, size, leafHash, pf, root); err != nil {
		return fault.Wrap(fault.Terminal, CodeInclusionInvalid, err)
	}
	return nil
}

// VerifyConsistency reports whether pf proves that a tree of size1 with head root1
// is a prefix of a tree of size2 with head root2.
func VerifyConsistency(size1, size2 uint64, root1, root2 []byte, pf [][]byte) error {
	if err := proof.VerifyConsistency(merkleHasher, size1, size2, pf, root1, root2); err != nil {
		return fault.Wrap(fault.Terminal, CodeConsistencyInvalid, err)
	}
	return nil
}

// Checkpoint is a commitment to the log at a point in time: the head root over a
// given number of leaves, scoped to an origin that names the log. It is the value a
// signer signs to make the root attributable and non-repudiable. The signing layer
// is added separately; on its own a Checkpoint is an unauthenticated root.
type Checkpoint struct {
	Origin   string
	Size     uint64
	RootHash []byte
}
