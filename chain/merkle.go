package chain

import (
	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"

	"github.com/ionalpha/flynn/fault"
)

// merkleHasher is the RFC 6962 SHA-256 hasher. Its leaf and node prefixes stop a
// second-preimage attack that swaps a leaf for an internal node.
var merkleHasher = rfc6962.DefaultHasher

// Merkle failure codes, matching the published Provetrail conformance registry.
const (
	CodeMissingNode        = "merkle.missing_node"
	CodeInclusionInvalid   = "merkle.inclusion_invalid"
	CodeConsistencyInvalid = "merkle.consistency_invalid"
)

// LeafHash returns the RFC 6962 leaf hash of [LeafInput], not of the raw bytes.
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

// Tree is an append-only RFC 6962 Merkle log over event leaf hashes, built on
// transparency-dev/merkle. Proof node hashes live in a [NodeStore], in memory or
// paged into tiles. It is not safe for concurrent mutation.
type Tree struct {
	factory *compact.RangeFactory
	rang    *compact.Range
	store   NodeStore
	leaves  uint64
}

// NewTree returns an empty Merkle log whose nodes are held in memory.
func NewTree() *Tree { return NewTreeWithStore(newMemNodeStore()) }

// NewTreeWithStore returns an empty Merkle log backed by store, so proof nodes
// need not all be resident.
func NewTreeWithStore(store NodeStore) *Tree {
	factory := &compact.RangeFactory{Hash: merkleHasher.HashChildren}
	return &Tree{
		factory: factory,
		rang:    factory.NewEmptyRange(0),
		store:   store,
	}
}

// LoadTree reopens a log of the given size from a store holding its nodes. It
// rebuilds the append frontier from them, so a restart does not refold the whole
// history. The size must be the authenticated leaf count from the latest signed
// checkpoint.
func LoadTree(store NodeStore, size uint64) (*Tree, error) {
	factory := &compact.RangeFactory{Hash: merkleHasher.HashChildren}
	ids := compact.RangeNodes(0, size, nil)
	hashes := make([][]byte, len(ids))
	for i, id := range ids {
		h, ok, err := store.Node(id.Level, id.Index)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fault.New(fault.Terminal, CodeMissingNode, "chain: stored log is missing a frontier node")
		}
		hashes[i] = h
	}
	rang, err := factory.NewRange(0, size, hashes)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return &Tree{factory: factory, rang: rang, store: store, leaves: size}, nil
}

// Append adds the next leaf and stores it with every internal node it completes,
// so proofs need no recomputation.
func (t *Tree) Append(canonical []byte) error {
	leaf, err := LeafHash(canonical)
	if err != nil {
		return err
	}
	if err := t.store.PutNode(0, t.leaves, leaf); err != nil {
		return err
	}
	var putErr error
	if err := t.rang.Append(leaf, func(id compact.NodeID, hash []byte) {
		if putErr == nil {
			putErr = t.store.PutNode(id.Level, id.Index, hash)
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

// Root returns the tree head; an empty tree gives SHA-256 of the empty string.
func (t *Tree) Root() ([]byte, error) {
	if t.leaves == 0 {
		return merkleHasher.EmptyRoot(), nil
	}
	root, err := t.rang.GetRootHash(nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return root, nil
}

// InclusionProof returns the audit path for the leaf at index.
func (t *Tree) InclusionProof(index uint64) ([][]byte, error) {
	nodes, err := proof.Inclusion(index, t.leaves)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeInclusionInvalid, err)
	}
	return t.assemble(nodes)
}

// ConsistencyProof proves the tree at the earlier size is a prefix of the
// current one.
func (t *Tree) ConsistencyProof(size uint64) ([][]byte, error) {
	nodes, err := proof.Consistency(size, t.leaves)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeConsistencyInvalid, err)
	}
	return t.assemble(nodes)
}

func (t *Tree) assemble(nodes proof.Nodes) ([][]byte, error) {
	return assembleProof(t.store, nodes)
}

// assembleProof rehashes a proof's nodes from a store, collapsing the ephemeral
// node the library may require. A sealed run uses it without a live Tree.
func assembleProof(store NodeStore, nodes proof.Nodes) ([][]byte, error) {
	hashes := make([][]byte, len(nodes.IDs))
	for i, id := range nodes.IDs {
		h, ok, err := store.Node(id.Level, id.Index)
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

// cloneStore snapshots the node store so a sealed run is isolated from later
// appends. Builders use in-memory stores, which can; a durable tree is
// checkpointed instead of sealed.
func (t *Tree) cloneStore() (NodeStore, error) {
	c, ok := t.store.(cloneableStore)
	if !ok {
		return nil, fault.New(fault.Terminal, CodeEncode, "chain: node store cannot be snapshotted for sealing")
	}
	return c.clone(), nil
}

// VerifyInclusion checks that pf proves leafHash is the leaf at index in a tree
// of the given size with head root. It needs no more than those and the proof.
func VerifyInclusion(index, size uint64, leafHash, root []byte, pf [][]byte) error {
	if err := proof.VerifyInclusion(merkleHasher, index, size, leafHash, pf, root); err != nil {
		return fault.Wrap(fault.Terminal, CodeInclusionInvalid, err)
	}
	return nil
}

// VerifyConsistency checks that pf proves the tree (size1, root1) is a prefix of
// (size2, root2).
func VerifyConsistency(size1, size2 uint64, root1, root2 []byte, pf [][]byte) error {
	if err := proof.VerifyConsistency(merkleHasher, size1, size2, pf, root1, root2); err != nil {
		return fault.Wrap(fault.Terminal, CodeConsistencyInvalid, err)
	}
	return nil
}

// Checkpoint commits to the log at a point in time: the head root over a number
// of leaves, scoped to an origin that names the log. Unsigned, it is an
// unauthenticated root.
type Checkpoint struct {
	Origin   string
	Size     uint64
	RootHash []byte
}
