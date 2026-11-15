package chain

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// snapshotContentType is in the protected header, so a snapshot signature cannot
// be replayed as a checkpoint signature or the reverse.
const snapshotContentType = "application/provetrail-snapshot+cbor"

// Snapshot failure codes, matching the published Provetrail conformance registry.
const (
	CodeSnapshotDecode    = "snap.decode"
	CodeSnapshotBinding   = "snap.binding_mismatch"
	CodeSnapshotStateHash = "snap.state_hash_mismatch"
	CodeSnapshotLogShort  = "snap.log_short"
	CodeSnapshotNoSigner  = "snap.no_signer"
)

// SnapshotClaim binds a projected state to the log prefix it derives from: the
// SHA-256 of the payload, the stream and Seq it is current as of, and the
// stream's Merkle checkpoint over that prefix. A verifier can restore from the
// signature and state hash, or re-fold the prefix and compare to the checkpoint.
type SnapshotClaim struct {
	Stream     string
	Seq        int64
	StateHash  []byte
	Checkpoint Checkpoint
}

// snapshotClaimPayload is the signed bytes, from the canonical encoder, so they
// are reproducible in any language.
func snapshotClaimPayload(c SnapshotClaim) ([]byte, error) {
	m := map[string]any{
		"origin":     c.Checkpoint.Origin,
		"stream":     c.Stream,
		"seq":        c.Seq,
		"state_hash": c.StateHash,
		"size":       c.Checkpoint.Size,
		"root":       c.Checkpoint.RootHash,
	}
	b, err := canonicalEnc.Marshal(m)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

func decodeSnapshotClaim(b []byte) (SnapshotClaim, error) {
	var raw struct {
		Origin    string `cbor:"origin"`
		Stream    string `cbor:"stream"`
		Seq       int64  `cbor:"seq"`
		StateHash []byte `cbor:"state_hash"`
		Size      uint64 `cbor:"size"`
		Root      []byte `cbor:"root"`
	}
	if err := canonicalDec.Unmarshal(b, &raw); err != nil {
		return SnapshotClaim{}, fault.Wrap(fault.Terminal, CodeSnapshotDecode, err)
	}
	return SnapshotClaim{
		Stream:     raw.Stream,
		Seq:        raw.Seq,
		StateHash:  raw.StateHash,
		Checkpoint: Checkpoint{Origin: raw.Origin, Size: raw.Size, RootHash: raw.Root},
	}, nil
}

// SnapshotSigner signs a snapshot claim, as [RootSigner] does a checkpoint.
// [Ed25519RootSigner] implements both.
type SnapshotSigner interface {
	SignSnapshotClaim(c SnapshotClaim) ([]byte, error)
	KeyID() string
}

// SignSnapshotClaim signs under the snapshot content type, so the signature
// binds the claim's meaning.
func (s *Ed25519RootSigner) SignSnapshotClaim(c SnapshotClaim) ([]byte, error) {
	payload, err := snapshotClaimPayload(c)
	if err != nil {
		return nil, err
	}
	headers := cose.Headers{
		Protected: cose.ProtectedHeader{
			cose.HeaderLabelAlgorithm:   checkpointAlg,
			cose.HeaderLabelContentType: snapshotContentType,
			cose.HeaderLabelKeyID:       []byte(s.keyID),
		},
	}
	msg, err := cose.Sign1(nil, s.signer, headers, payload, nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSign, err)
	}
	return msg, nil
}

var _ SnapshotSigner = (*Ed25519RootSigner)(nil)

// VerifySnapshotClaim fails closed like [VerifyCheckpoint], and also rejects a
// checkpoint signature presented as a snapshot.
func VerifySnapshotClaim(coseBytes []byte, ring *RootKeyring) (SnapshotClaim, error) {
	payload, err := verifiedPayload(coseBytes, ring, snapshotContentType, "snapshot")
	if err != nil {
		return SnapshotClaim{}, err
	}
	return decodeSnapshotClaim(payload)
}

// snapshotWire is the stored form: the projection payload plus its signed claim.
type snapshotWire struct {
	State []byte `cbor:"state"`
	COSE  []byte `cbor:"cose"`
}

// SnapshotSealer implements [spine.SnapshotCodec], so a store never persists or
// trusts an unsigned state blob. Seal binds the state to the stream's Merkle
// checkpoint under the instance key; Open rejects anything the keyring does not
// vouch for, and a rebuild then falls back to a full fold.
type SnapshotSealer struct {
	signer SnapshotSigner
	ring   *RootKeyring
	origin OriginFunc
}

// NewSnapshotSealer verifies against ring and signs with signer. A nil signer
// gives a verify-only sealer (Seal refuses) for a process holding no key. A nil
// originFor uses the stream id.
func NewSnapshotSealer(signer SnapshotSigner, ring *RootKeyring, originFor OriginFunc) (*SnapshotSealer, error) {
	if ring == nil {
		return nil, fault.New(fault.Terminal, CodeSignerKey, "chain: snapshot sealer needs a keyring")
	}
	return &SnapshotSealer{signer: signer, ring: ring, origin: originFor}, nil
}

func (ss *SnapshotSealer) originFor(stream string) string {
	if ss.origin != nil {
		return ss.origin(stream)
	}
	return stream
}

// Seal signs a claim binding the snapshot to the stream's Merkle checkpoint at
// its Seq. It folds the stream up to Seq into a tree, linear in the prefix: a
// write-side cost per snapshot, bounded by segment rotation, so reads stop
// growing with history.
func (ss *SnapshotSealer) Seal(ctx context.Context, log spine.Log, s spine.Snapshot) (spine.Snapshot, error) {
	if ss.signer == nil {
		return spine.Snapshot{}, fault.New(fault.Terminal, CodeSnapshotNoSigner, "chain: snapshot sealer has no signer")
	}
	events, err := log.Read(ctx, spine.Query{Stream: s.Stream})
	if err != nil {
		return spine.Snapshot{}, err
	}
	tree := NewTree()
	var lastSeq int64
	for _, e := range events {
		if e.Seq > s.Seq {
			break
		}
		canonical, err := CanonicalBytes(e)
		if err != nil {
			return spine.Snapshot{}, err
		}
		if err := tree.Append(canonical); err != nil {
			return spine.Snapshot{}, err
		}
		lastSeq = e.Seq
	}
	if lastSeq != s.Seq {
		return spine.Snapshot{}, fault.New(fault.Terminal, CodeSnapshotLogShort,
			"chain: stream does not reach the snapshot's seq")
	}
	root, err := tree.Root()
	if err != nil {
		return spine.Snapshot{}, err
	}
	stateHash := sha256.Sum256(s.Payload)
	claim := SnapshotClaim{
		Stream:    s.Stream,
		Seq:       s.Seq,
		StateHash: stateHash[:],
		Checkpoint: Checkpoint{
			Origin:   ss.originFor(s.Stream),
			Size:     tree.Size(),
			RootHash: root,
		},
	}
	sig, err := ss.signer.SignSnapshotClaim(claim)
	if err != nil {
		return spine.Snapshot{}, err
	}
	sealed, err := canonicalEnc.Marshal(snapshotWire{State: s.Payload, COSE: sig})
	if err != nil {
		return spine.Snapshot{}, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return spine.Snapshot{Stream: s.Stream, Seq: s.Seq, Payload: sealed}, nil
}

// Open returns the inner projection payload. It fails closed on a payload that
// is not a sealed envelope, a signature the keyring does not vouch for, a claim
// bound to another stream or seq, or a state hash that differs from the claim.
// The caller then falls back to a full fold, which is slower, never wrong.
func (ss *SnapshotSealer) Open(_ context.Context, s spine.Snapshot) (spine.Snapshot, error) {
	var wire snapshotWire
	if err := canonicalDec.Unmarshal(s.Payload, &wire); err != nil {
		return spine.Snapshot{}, fault.Wrap(fault.Terminal, CodeSnapshotDecode, err)
	}
	claim, err := VerifySnapshotClaim(wire.COSE, ss.ring)
	if err != nil {
		return spine.Snapshot{}, err
	}
	if claim.Stream != s.Stream || claim.Seq != s.Seq {
		return spine.Snapshot{}, fault.New(fault.Terminal, CodeSnapshotBinding,
			"chain: snapshot claim is bound to a different stream or seq")
	}
	stateHash := sha256.Sum256(wire.State)
	if !bytes.Equal(stateHash[:], claim.StateHash) {
		return spine.Snapshot{}, fault.New(fault.Terminal, CodeSnapshotStateHash,
			"chain: snapshot state does not match its signed hash")
	}
	return spine.Snapshot{Stream: s.Stream, Seq: s.Seq, Payload: wire.State}, nil
}

var _ spine.SnapshotCodec = (*SnapshotSealer)(nil)
