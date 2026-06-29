package chain

import (
	"bytes"

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

// requireCanonical reports an error unless raw is the exact canonical encoding of
// decoded. Re-encoding the decoded value and comparing rejects any mutation of the
// artifact's framing, a reordered, renamed, dropped, or extra field, that a lenient
// decode would otherwise absorb into a default value. The cryptographic checks bind
// the contents; this binds the container.
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

// Builder accumulates a run's events as canonical encodings and a Merkle log over
// them, so the run can be sealed into a signed, independently verifiable record. It
// composes the canonical encoding, the structural ordering, and the tamper-evident
// log into one producer. A Builder is not safe for concurrent use.
type Builder struct {
	origin string
	tree   *Tree
	events [][]byte
}

// NewBuilder starts a record builder for a run identified by origin.
func NewBuilder(origin string) *Builder {
	return &Builder{origin: origin, tree: NewTree()}
}

// Add canonicalizes e, appends it to the log as the next leaf, and retains the
// canonical bytes for the sealed record.
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

// Seal signs the current Merkle head and returns the sealed run: the signed
// checkpoint plus every event's canonical bytes, which together are sufficient for
// full verification and for extracting a single-event proof. An empty run is
// refused so a record always attests at least one event.
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
	return &SealedRun{cose: sc.COSE, events: b.events, tree: b.tree}, nil
}

// SealedRun is a signed, verifiable record of a run: the COSE-signed checkpoint over
// the Merkle head plus the canonical bytes of every event. It can be marshalled to a
// portable artifact, verified in full, or used to produce a standalone proof for one
// event.
type SealedRun struct {
	cose   []byte
	events [][]byte
	tree   *Tree
}

type sealedRunWire struct {
	Checkpoint []byte   `cbor:"checkpoint"`
	Events     [][]byte `cbor:"events"`
}

// Marshal encodes the sealed run as a portable, deterministic CBOR artifact a third
// party can verify with VerifyRun.
func (s *SealedRun) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(sealedRunWire{Checkpoint: s.cose, Events: s.events})
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// EventProof extracts a standalone proof that the event at index is in the sealed
// run, verifiable on its own with VerifyEventProof and without the other events.
func (s *SealedRun) EventProof(index uint64) (*EventProof, error) {
	if index >= uint64(len(s.events)) {
		return nil, fault.New(fault.Terminal, CodeIndexRange, "chain: event index out of range")
	}
	inclusion, err := s.tree.InclusionProof(index)
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

// EventProof is a standalone proof that one event is included in a signed run: the
// event's canonical bytes, its position, the inclusion path, and the signed
// checkpoint whose root the path reconstructs. It is the portable artifact a third
// party checks to confirm a single action happened under a signed log, without the
// rest of the run.
type EventProof struct {
	Canonical  []byte   `cbor:"canonical"`
	Index      uint64   `cbor:"index"`
	Size       uint64   `cbor:"size"`
	Inclusion  [][]byte `cbor:"inclusion"`
	Checkpoint []byte   `cbor:"checkpoint"`
}

// Marshal encodes the proof as a portable, deterministic CBOR artifact.
func (p *EventProof) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(p)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// VerifyRun verifies a marshalled sealed run against the keyring and returns its
// events in order. It checks every layer and fails closed on the first fault:
//   - the checkpoint signature is valid and from an authorized key,
//   - the event count equals the signed size,
//   - every event is in canonical form and the stream is strictly ordered,
//   - the events rebuild exactly the signed Merkle root.
//
// Passing it means an independent party, trusting only the signing key, can rely on
// the whole event sequence being authentic and untampered.
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

// VerifyEventProof verifies a marshalled single-event proof against the keyring and
// returns the event. It checks every layer and fails closed: the checkpoint
// signature is valid and authorized, the proof's size matches the signed size, the
// event is in canonical form, and the event's leaf is included under the signed root
// at the claimed index. It needs only the proof and the keyring, not the run.
func VerifyEventProof(proof []byte, ring *RootKeyring) (spine.Event, error) {
	var p EventProof
	if err := canonicalDec.Unmarshal(proof, &p); err != nil {
		return spine.Event{}, fault.Wrap(fault.Terminal, CodeRecordDecode, err)
	}
	if err := requireCanonical(p, proof); err != nil {
		return spine.Event{}, err
	}
	cp, err := VerifyCheckpoint(p.Checkpoint, ring)
	if err != nil {
		return spine.Event{}, err
	}
	if p.Size != cp.Size {
		return spine.Event{}, fault.New(fault.Terminal, CodeSizeMismatch, "chain: proof size does not match the signed size")
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
