package chain

import (
	"github.com/ionalpha/flynn/fault"
)

// ConsistencyProof shows a log only grew between two signed checkpoints: the
// earlier tree is a prefix of the later, with nothing rewritten or removed. It
// carries both checkpoints and the RFC 6962 path connecting their roots, so no
// events are needed.
type ConsistencyProof struct {
	Before []byte   `cbor:"before"`
	After  []byte   `cbor:"after"`
	Proof  [][]byte `cbor:"proof"`
}

// Marshal encodes the proof as deterministic CBOR.
func (p *ConsistencyProof) Marshal() ([]byte, error) {
	b, err := canonicalEnc.Marshal(p)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// VerifyConsistencyProof returns the two checkpoints, earlier first, failing
// closed unless the proof is canonical, both checkpoints are signed by an
// authorized key, and the path proves the earlier root a prefix of the later.
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
