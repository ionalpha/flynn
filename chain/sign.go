package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"io"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// checkpointAlg is Ed25519 as COSE algorithm -19 (RFC 9864), which replaced the
// deprecated EdDSA (-8). checkpointContentType is in the protected header so the
// signature binds the payload's meaning; it is in the vendor tree because an
// unfaceted application/provetrail-* name is not obtainable (RFC 6838 3.1).
//
// go-cose v1.3.0 rejects -19, so ed25519CoseSigner and ed25519CoseVerifier carry
// the label; the signature is plain Ed25519.
const (
	checkpointAlg         = cose.Algorithm(-19)
	checkpointContentType = "application/vnd.provetrail.checkpoint+cbor"
)

// Signing failure codes, matching the published Provetrail conformance registry.
const (
	CodeSignerEmptyKeyID = "sign.empty_key_id"
	CodeSignerKey        = "sign.bad_key"
	CodeSign             = "sign.sign_failed"
	CodeSignatureInvalid = "sign.signature_invalid"
	CodeUnknownKey       = "sign.unknown_key"
	CodeContentType      = "sign.bad_content_type"
	CodeCheckpointDecode = "sign.checkpoint_decode"
)

// RootSigner signs a Merkle head; a local key, KMS or hardware token all fit,
// and a verifier does not depend on which.
type RootSigner interface {
	SignCheckpoint(c Checkpoint) (SignedCheckpoint, error)
	KeyID() string
}

// SignedCheckpoint pairs a checkpoint with its COSE_Sign1 signature. COSE is the
// authoritative artifact a verifier checks; Checkpoint is a convenience copy,
// never trusted over the signed payload.
type SignedCheckpoint struct {
	Checkpoint Checkpoint
	KeyID      string
	COSE       []byte
}

// checkpointPayload is the signed bytes, from the canonical event encoder, so
// they are reproducible in any language.
func checkpointPayload(c Checkpoint) ([]byte, error) {
	if len(c.RootHash) != sha256.Size {
		return nil, fault.New(fault.Terminal, CodeEncode, "chain: checkpoint root is not a SHA-256 digest")
	}
	m := map[string]any{
		"origin": c.Origin,
		"size":   c.Size,
		"root":   c.RootHash,
	}
	b, err := canonicalEnc.Marshal(m)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

func decodeCheckpoint(b []byte) (Checkpoint, error) {
	var raw struct {
		Origin string `cbor:"origin"`
		Size   uint64 `cbor:"size"`
		Root   []byte `cbor:"root"`
	}
	if err := canonicalDec.Unmarshal(b, &raw); err != nil {
		return Checkpoint{}, fault.Wrap(fault.Terminal, CodeCheckpointDecode, err)
	}
	// Re-encode and compare, so a missing, extra or reordered field is rejected
	// instead of defaulted. A short root must not verify against a truncated
	// tree head.
	re, err := canonicalEnc.Marshal(map[string]any{"origin": raw.Origin, "size": raw.Size, "root": raw.Root})
	if err != nil || !bytes.Equal(re, b) {
		return Checkpoint{}, fault.New(fault.Terminal, CodeCheckpointDecode, "chain: checkpoint payload is not in canonical form")
	}
	if len(raw.Root) != sha256.Size {
		return Checkpoint{}, fault.New(fault.Terminal, CodeCheckpointDecode, "chain: checkpoint root is not a SHA-256 digest")
	}
	return Checkpoint{Origin: raw.Origin, Size: raw.Size, RootHash: raw.Root}, nil
}

// Ed25519RootSigner is the default signer, with no external signing service.
type Ed25519RootSigner struct {
	keyID  string
	signer cose.Signer
}

// NewEd25519RootSigner refuses an empty key id or a malformed key.
func NewEd25519RootSigner(keyID string, priv ed25519.PrivateKey) (*Ed25519RootSigner, error) {
	if keyID == "" {
		return nil, fault.New(fault.Terminal, CodeSignerEmptyKeyID, "chain: root signer key id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, CodeSignerKey, "chain: malformed Ed25519 private key")
	}
	return &Ed25519RootSigner{keyID: keyID, signer: ed25519CoseSigner{key: priv}}, nil
}

// ed25519CoseSigner exists because go-cose v1.3.0 dispatches only -8.
type ed25519CoseSigner struct{ key ed25519.PrivateKey }

func (s ed25519CoseSigner) Algorithm() cose.Algorithm { return checkpointAlg }

func (s ed25519CoseSigner) Sign(_ io.Reader, content []byte) ([]byte, error) {
	return ed25519.Sign(s.key, content), nil
}

// ed25519CoseVerifier is the verifying half of [ed25519CoseSigner].
type ed25519CoseVerifier struct{ key ed25519.PublicKey }

func (v ed25519CoseVerifier) Algorithm() cose.Algorithm { return checkpointAlg }

func (v ed25519CoseVerifier) Verify(content, signature []byte) error {
	if !ed25519.Verify(v.key, content, signature) {
		return cose.ErrVerification
	}
	return nil
}

// KeyID returns the key identifier this signer stamps on checkpoints.
func (s *Ed25519RootSigner) KeyID() string { return s.keyID }

// SignCheckpoint puts the algorithm, content type and key id in the protected
// header, so the signature covers all three. Ed25519 is deterministic.
func (s *Ed25519RootSigner) SignCheckpoint(c Checkpoint) (SignedCheckpoint, error) {
	payload, err := checkpointPayload(c)
	if err != nil {
		return SignedCheckpoint{}, err
	}
	headers := cose.Headers{
		Protected: cose.ProtectedHeader{
			cose.HeaderLabelAlgorithm:   checkpointAlg,
			cose.HeaderLabelContentType: checkpointContentType,
			cose.HeaderLabelKeyID:       []byte(s.keyID),
		},
	}
	msg, err := cose.Sign1(nil, s.signer, headers, payload, nil)
	if err != nil {
		return SignedCheckpoint{}, fault.Wrap(fault.Terminal, CodeSign, err)
	}
	return SignedCheckpoint{Checkpoint: c, KeyID: s.keyID, COSE: msg}, nil
}

var _ RootSigner = (*Ed25519RootSigner)(nil)

// RootKeyring holds the public keys authorized to sign checkpoints, by key id;
// revoking a signer removes its key. Treat it as read-only after construction.
type RootKeyring struct {
	keys map[string]ed25519.PublicKey
}

// NewRootKeyring returns an empty keyring.
func NewRootKeyring() *RootKeyring { return &RootKeyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers a key; adding the same id again rotates it. It refuses an empty
// id or malformed key.
func (k *RootKeyring) Add(keyID string, pub ed25519.PublicKey) error {
	if keyID == "" {
		return fault.New(fault.Terminal, CodeSignerEmptyKeyID, "chain: keyring id must not be empty")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fault.New(fault.Terminal, CodeSignerKey, "chain: malformed Ed25519 public key for "+keyID)
	}
	k.keys[keyID] = pub
	return nil
}

// VerifyCheckpoint fails closed on an unknown key, wrong content type, bad
// signature, unexpected algorithm or undecodable payload. The result comes from
// the signed payload, never an unsigned copy.
func VerifyCheckpoint(coseBytes []byte, ring *RootKeyring) (Checkpoint, error) {
	payload, err := verifiedPayload(coseBytes, ring, checkpointContentType, "checkpoint")
	if err != nil {
		return Checkpoint{}, err
	}
	return decodeCheckpoint(payload)
}

// verifiedPayload backs [VerifyCheckpoint] and [VerifySnapshotClaim]. The
// content-type check keeps them apart: both sign with the same keys, so without
// it a checkpoint signature would verify as a snapshot claim. subject names the
// artefact in errors.
func verifiedPayload(coseBytes []byte, ring *RootKeyring, wantType, subject string) ([]byte, error) {
	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(coseBytes); err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	if ct, _ := msg.Headers.Protected[cose.HeaderLabelContentType].(string); ct != wantType {
		return nil, fault.New(fault.Terminal, CodeContentType, "chain: unexpected "+subject+" content type")
	}
	kid, _ := msg.Headers.Protected[cose.HeaderLabelKeyID].([]byte)
	pub, ok := ring.keys[string(kid)]
	if !ok {
		return nil, fault.New(fault.Terminal, CodeUnknownKey, "chain: "+subject+" signed by an unknown key")
	}
	if err := msg.Verify(nil, ed25519CoseVerifier{key: pub}); err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	return msg.Payload, nil
}
