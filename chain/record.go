package chain

import (
	"bytes"

	"github.com/transparency-dev/merkle/proof"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// Record-composition failure codes, matching the published Provetrail registry.
const (
	CodeEmptyRecord  = "record.empty"
	CodeSizeMismatch = "record.size_mismatch"
	CodeRootMismatch = "record.root_mismatch"
	CodeRecordDecode = "record.decode"
	CodeIndexRange   = "record.index_out_of_range"
	CodeNonCanonical = "record.non_canonical"
)

// requireCanonical rejects a reordered, renamed, dropped or extra field that a
// lenient decode would absorb into a default. The cryptographic checks bind the
// contents; this binds the container.
func requireCanonical(decoded any, raw []byte) error {
	re, err := canonicalEnc.Marshal(decoded)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	if !bytes.Equal(re, raw) {
		return fault.New(fault.Terminal, CodeNonCanonical, "chain: record is not in canonical form")
	}
	return nil
}

// Builder accumulates a run's canonical events and a Merkle log over them, to be
// sealed into a signed record. It is not safe for concurrent use.
type Builder struct {
	origin string
	tree   *Tree
	events [][]byte
}

// NewBuilder starts a builder for the run identified by origin.
func NewBuilder(origin string) *Builder {
	return &Builder{origin: origin, tree: NewTree()}
}

// Add appends e as the next leaf and retains its canonical bytes.
func (b *Builder) Add(e spine.Event) error {
	cb, err := CanonicalBytes(e)
	if err != nil {
		return err
	}
	if err := b.tree.Append(cb); err != nil {
		return err
	}
	b.events = append(b.events, cb)
	return nil
}

// Seal signs the current Merkle head. An empty run is refused so a record
// attests at least one event.
func (b *Builder) Seal(signer RootSigner) (*SealedRun, error) {
	if len(b.events) == 0 {
		return nil, fault.New(fault.Terminal, CodeEmptyRecord, "chain: cannot seal a run with no events")
	}
	root, err := b.tree.Root()
	if err != nil {
		return nil, err
	}
	cp := Checkpoint{Origin: b.origin, Size: b.tree.Size(), RootHash: root}
	sc, err := signer.SignCheckpoint(cp)
	if err != nil {
		return nil, err
	}
	events := make([][]byte, len(b.events))
	copy(events, b.events)
	nodes, err := b.tree.cloneStore()
	if err != nil {
		return nil, err
	}
	return &SealedRun{cose: sc.COSE, events: events, nodes: nodes}, nil
}

// SealAndReset seals the run and starts an empty one under the same origin. It
// is the rotation point for a long-lived stream: events and Merkle nodes move
// into the returned record, so retention is bounded per segment. Each segment
// verifies on its own; Seq continuity ties them together.
func (b *Builder) SealAndReset(signer RootSigner) (*SealedRun, error) {
	sealed, err := b.Seal(signer)
	if err != nil {
		return nil, err
	}
	b.tree = NewTree()
	b.events = nil
	return sealed, nil
}

// SealedRun is an immutable record: the signed checkpoint plus every event's
// canonical bytes. Later appends to its builder do not affect it.
type SealedRun struct {
	cose   []byte
	events [][]byte
	// nodes lets EventProof look up an inclusion path instead of rebuilding the
	// tree. Seal always populates it.
	nodes NodeStore
}

type sealedRunWire struct {
	Checkpoint []byte   `cbor:"checkpoint"`
	Events     [][]byte `cbor:"events"`
}

// Marshal encodes the run as deterministic CBOR for [VerifyRun].
func (s *SealedRun) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(sealedRunWire{Checkpoint: s.cose, Events: s.events})
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// EventProof extracts a proof for the event at index, checkable with
// [VerifyEventProof] without the other events.
func (s *SealedRun) EventProof(index uint64) (*EventProof, error) {
	if index >= uint64(len(s.events)) {
		return nil, fault.New(fault.Terminal, CodeIndexRange, "chain: event index out of range")
	}
	nodes, err := proof.Inclusion(index, uint64(len(s.events)))
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeInclusionInvalid, err)
	}
	inclusion, err := assembleProof(s.nodes, nodes)
	if err != nil {
		return nil, err
	}
	return &EventProof{
		Canonical:  s.events[index],
		Index:      index,
		Size:       uint64(len(s.events)),
		Inclusion:  inclusion,
		Checkpoint: s.cose,
	}, nil
}

// EventProof shows one event is included in a signed run: its canonical bytes,
// position, the inclusion path, and the signed checkpoint whose root the path
// reconstructs.
type EventProof struct {
	Canonical  []byte   `cbor:"canonical"`
	Index      uint64   `cbor:"index"`
	Size       uint64   `cbor:"size"`
	Inclusion  [][]byte `cbor:"inclusion"`
	Checkpoint []byte   `cbor:"checkpoint"`
}

// Marshal encodes the proof as deterministic CBOR.
func (p *EventProof) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(p)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// VerifyRun returns the events of a marshalled run, failing closed on the first
// fault:
//   - the checkpoint signature is valid and from an authorized key,
//   - the event count equals the signed size,
//   - every event is canonical and the stream is strictly ordered,
//   - the events rebuild exactly the signed Merkle root.
func VerifyRun(record []byte, ring *RootKeyring) ([]spine.Event, error) {
	var w sealedRunWire
	if err := canonicalDec.Unmarshal(record, &w); err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeRecordDecode, err)
	}
	if err := requireCanonical(w, record); err != nil {
		return nil, err
	}
	cp, err := VerifyCheckpoint(w.Checkpoint, ring)
	if err != nil {
		return nil, err
	}
	// Checked before the size comparison so a signed size-0 checkpoint reports
	// emptiness, not a vacuous size match.
	if len(w.Events) == 0 {
		return nil, fault.New(fault.Terminal, CodeEmptyRecord, "chain: record carries no events")
	}
	if uint64(len(w.Events)) != cp.Size {
		return nil, fault.New(fault.Terminal, CodeSizeMismatch, "chain: event count does not match the signed size")
	}
	events, err := NewVerifier().VerifyStream(w.Events)
	if err != nil {
		return nil, err
	}
	tree := NewTree()
	for _, cb := range w.Events {
		if err := tree.Append(cb); err != nil {
			return nil, err
		}
	}
	root, err := tree.Root()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(root, cp.RootHash) {
		return nil, fault.New(fault.Terminal, CodeRootMismatch, "chain: events do not reproduce the signed root")
	}
	return events, nil
}

// VerifyEventProof returns the event of a marshalled proof, failing closed unless
// the checkpoint signature is authorized, the size matches the signed size, the
// event is canonical, and its leaf is included under the signed root at the
// claimed index.
func VerifyEventProof(proofBytes []byte, ring *RootKeyring) (spine.Event, error) {
	var p EventProof
	if err := canonicalDec.Unmarshal(proofBytes, &p); err != nil {
		return spine.Event{}, fault.Wrap(fault.Terminal, CodeRecordDecode, err)
	}
	if err := requireCanonical(p, proofBytes); err != nil {
		return spine.Event{}, err
	}
	cp, err := VerifyCheckpoint(p.Checkpoint, ring)
	if err != nil {
		return spine.Event{}, err
	}
	if p.Size != cp.Size {
		return spine.Event{}, fault.New(fault.Terminal, CodeSizeMismatch, "chain: proof size does not match the signed size")
	}
	if p.Index >= p.Size {
		return spine.Event{}, fault.New(fault.Terminal, CodeIndexRange, "chain: proof index is outside the signed tree")
	}
	// Name a missing node instead of a generic inclusion failure. The expected
	// length is what Rehash yields, which can be below the ID count when an
	// ephemeral range collapses to one hash.
	if nodes, err := proof.Inclusion(p.Index, p.Size); err == nil {
		dummies := make([][]byte, len(nodes.IDs))
		for i := range dummies {
			dummies[i] = make([]byte, 32)
		}
		if shaped, err := nodes.Rehash(dummies, merkleHasher.HashChildren); err == nil && len(p.Inclusion) < len(shaped) {
			return spine.Event{}, fault.New(fault.Terminal, CodeMissingNode, "chain: inclusion path is missing a node")
		}
	}
	if err := VerifyCanonical(p.Canonical); err != nil {
		return spine.Event{}, err
	}
	leaf, err := LeafHash(p.Canonical)
	if err != nil {
		return spine.Event{}, err
	}
	if err := VerifyInclusion(p.Index, p.Size, leaf, cp.RootHash, p.Inclusion); err != nil {
		return spine.Event{}, err
	}
	return DecodeCanonical(p.Canonical)
}
