package chain

import (
	"github.com/ionalpha/flynn/fault"
)

// ConsistencyProof is a standalone proof that an append-only log only ever grew
// between two signed checkpoints: the earlier checkpoint's tree is a prefix of the
// later one's, with no event rewritten or removed in between. It carries both signed
// checkpoints and the RFC 9162 consistency path that connects their roots. It is the
// portable artifact a third party checks to confirm a log was not forked or rewritten
// across two points in time, without needing the events themselves.
type ConsistencyProof struct {
	Before []byte   `cbor:"before"`
	After  []byte   `cbor:"after"`
	Proof  [][]byte `cbor:"proof"`
}

// Marshal encodes the proof as a portable, deterministic CBOR artifact.
func (p *ConsistencyProof) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(p)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// VerifyConsistencyProof verifies a marshalled consistency proof against the keyring
// and returns the two checkpoints it connects, earlier first. It checks every layer
// and fails closed: the proof is in canonical form, both checkpoints are validly
// signed by an authorized key, and the consistency path proves the earlier signed
// root is a prefix of the later signed root. Passing it means an independent party,
// trusting only the signing key, can rely on the log having only appended between the
// two checkpoints.
func VerifyConsistencyProof(proof []byte, ring *RootKeyring) (before, after Checkpoint, err error) {
	var p ConsistencyProof
	if derr := canonicalDec.Unmarshal(proof, &p); derr != nil {
		return Checkpoint{}, Checkpoint{}, fault.Wrap(fault.Terminal, CodeRecordDecode, derr)
	}
	if cerr := requireCanonical(p, proof); cerr != nil {
		return Checkpoint{}, Checkpoint{}, cerr
	}
	before, err = VerifyCheckpoint(p.Before, ring)
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	after, err = VerifyCheckpoint(p.After, ring)
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	if err = VerifyConsistency(before.Size, after.Size, before.RootHash, after.RootHash, p.Proof); err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	return before, after, nil
}
