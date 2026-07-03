package chain

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// snapshotContentType marks a COSE payload as a snapshot claim, so a snapshot
// signature can never be replayed as a checkpoint signature or vice versa: the
// content type is in the protected header and covered by the signature.
const snapshotContentType = "application/provetrail-snapshot+cbor"

// Snapshot failure codes, matching the published Provetrail conformance registry.
const (
	CodeSnapshotDecode    = "snap.decode"
	CodeSnapshotBinding   = "snap.binding_mismatch"
	CodeSnapshotStateHash = "snap.state_hash_mismatch"
	CodeSnapshotLogShort  = "snap.log_short"
	CodeSnapshotNoSigner  = "snap.no_signer"
)

// SnapshotClaim binds a projected state to the exact log prefix it derives from:
// the SHA-256 of the snapshot payload, the stream and sequence the projection is
// current as of, and the stream's Merkle checkpoint over that prefix. Signed, it
// makes a snapshot exactly as trustworthy as the log it summarizes - a verifier
// checks the signature and state hash to restore fast, and can always re-fold the
// prefix and compare against the checkpoint to audit in depth.
type SnapshotClaim struct {
	Stream     string
	Seq        int64
	StateHash  []byte
	Checkpoint Checkpoint
}

// snapshotClaimPayload is the deterministic CBOR encoding of a claim: the bytes
// that are signed. It uses the same canonical encoder as events and checkpoints,
// so the signed payload is reproducible in any language.
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

// SnapshotSigner produces a signed snapshot claim. Like RootSigner it is a port:
// the signing method is an implementation detail, so a verifier checks a snapshot
// without depending on how it was signed. Ed25519RootSigner implements both.
type SnapshotSigner interface {
	// SignSnapshotClaim signs c and returns the tagged COSE_Sign1 message.
	SignSnapshotClaim(c SnapshotClaim) ([]byte, error)
	// KeyID identifies the signing key in a verifier's keyring.
	KeyID() string
}

// SignSnapshotClaim signs a snapshot claim as COSE_Sign1 under the snapshot
// content type, so the signature binds the claim's meaning as well as its bytes.
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

// VerifySnapshotClaim verifies a COSE_Sign1 signed snapshot claim against the
// keyring and returns the claim it attests. It fails closed exactly like
// VerifyCheckpoint, and additionally rejects a checkpoint signature presented as a
// snapshot (the content types differ).
func VerifySnapshotClaim(coseBytes []byte, ring *RootKeyring) (SnapshotClaim, error) {
	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(coseBytes); err != nil {
		return SnapshotClaim{}, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	if ct, _ := msg.Headers.Protected[cose.HeaderLabelContentType].(string); ct != snapshotContentType {
		return SnapshotClaim{}, fault.New(fault.Terminal, CodeContentType, "chain: unexpected snapshot content type")
	}
	kid, _ := msg.Headers.Protected[cose.HeaderLabelKeyID].([]byte)
	pub, ok := ring.keys[string(kid)]
	if !ok {
		return SnapshotClaim{}, fault.New(fault.Terminal, CodeUnknownKey, "chain: snapshot signed by an unknown key")
	}
	verifier, err := cose.NewVerifier(checkpointAlg, pub)
	if err != nil {
		return SnapshotClaim{}, fault.Wrap(fault.Terminal, CodeSignerKey, err)
	}
	if err := msg.Verify(nil, verifier); err != nil {
		return SnapshotClaim{}, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	return decodeSnapshotClaim(msg.Payload)
}

// snapshotWire is the stored form of a verified snapshot: the projection payload
// plus the signed claim binding it to the log prefix it derives from.
type snapshotWire struct {
	State []byte `cbor:"state"`
	COSE  []byte `cbor:"cose"`
}

// SnapshotSealer seals projection snapshots into verified, checkpoint-bound
// artifacts and opens (verifies) them on read. It implements spine.SnapshotCodec,
// so a store that snapshots through it never persists or trusts an unsigned state
// blob: Seal binds the state to the stream's Merkle checkpoint under the instance
// key, and Open rejects anything the keyring does not vouch for, so a rebuild
// falls back to a full fold rather than restoring unverified state.
type SnapshotSealer struct {
	signer SnapshotSigner
	ring   *RootKeyring
	origin OriginFunc
}

// NewSnapshotSealer builds a sealer verifying against ring and signing with
// signer. A nil signer makes a verify-only sealer (Open works, Seal refuses), for
// a process that reads snapshots but holds no key. originFor maps a stream to its
// checkpoint origin; if nil, the stream id is used.
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

// Seal wraps a projection snapshot in a signed claim binding it to the stream's
// Merkle checkpoint at the snapshot's Seq. It folds the stream's canonical events
// up to Seq into the tree, which is linear in the prefix - a write-side cost paid
// once per snapshot, amortized by the cadence, and bounded by segment rotation;
// the read side is what stops growing with history.
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

// Open verifies a sealed snapshot and returns it with the inner projection
// payload. It fails closed: a payload that is not a sealed envelope, a signature
// the keyring does not vouch for, a claim bound to a different stream or seq, or a
// state whose hash does not match the signed claim are all rejected - the caller
// falls back to a full fold, which is only slower, never wrong.
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
