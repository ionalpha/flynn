package chain

import (
	"crypto/ed25519"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// checkpointAlg is the signature algorithm for a signed checkpoint: Ed25519 as
// EdDSA, the instance's identity key type. checkpointContentType is set in the
// protected header so the signature also binds the payload's meaning, not just its
// bytes.
const (
	checkpointAlg         = cose.AlgorithmEdDSA
	checkpointContentType = "application/provetrail-checkpoint+cbor"
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

// RootSigner produces a signed checkpoint over the Merkle head. The method (a local
// Ed25519 key, a KMS, a hardware token) is an implementation detail behind this
// port, so a verifier checks a signature without depending on how it was produced.
type RootSigner interface {
	// SignCheckpoint signs c and returns it with a detached COSE signature.
	SignCheckpoint(c Checkpoint) (SignedCheckpoint, error)
	// KeyID identifies the signing key in a verifier's keyring.
	KeyID() string
}

// SignedCheckpoint is a checkpoint plus a COSE_Sign1 signature over its canonical
// encoding, identified by the signing key. COSE holds the tagged COSE_Sign1
// message, which is the authoritative, self-describing artifact a verifier checks;
// the parsed Checkpoint is carried for convenience and is never trusted over the
// signed payload.
type SignedCheckpoint struct {
	Checkpoint Checkpoint
	KeyID      string
	COSE       []byte
}

// checkpointPayload is the deterministic CBOR encoding of a checkpoint: the bytes
// that are signed. It uses the same canonical encoder as events, so the signed
// payload is reproducible in any language.
func checkpointPayload(c Checkpoint) ([]byte, error) {
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
	return Checkpoint{Origin: raw.Origin, Size: raw.Size, RootHash: raw.Root}, nil
}

// Ed25519RootSigner signs checkpoints with an Ed25519 private key as COSE_Sign1. It
// is the default signer: the standard-library key type, no external signing service.
type Ed25519RootSigner struct {
	keyID  string
	signer cose.Signer
}

// NewEd25519RootSigner builds a signer over an existing Ed25519 private key. It
// refuses an empty key id or a malformed key so a signer is never silently unable
// to sign or unattributable.
func NewEd25519RootSigner(keyID string, priv ed25519.PrivateKey) (*Ed25519RootSigner, error) {
	if keyID == "" {
		return nil, fault.New(fault.Terminal, CodeSignerEmptyKeyID, "chain: root signer key id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, CodeSignerKey, "chain: malformed Ed25519 private key")
	}
	signer, err := cose.NewSigner(checkpointAlg, priv)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSignerKey, err)
	}
	return &Ed25519RootSigner{keyID: keyID, signer: signer}, nil
}

// KeyID identifies the signing key.
func (s *Ed25519RootSigner) KeyID() string { return s.keyID }

// SignCheckpoint signs c. The algorithm, content type, and key id go in the
// protected header, so all three are covered by the signature and cannot be altered
// without detection. Ed25519 signing is deterministic, so no randomness is needed.
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

// RootKeyring is the set of public keys authorized to sign checkpoints, keyed by
// key id. Only a signature from a key in the ring counts, so revoking a signer is
// removing its key. It is read-only after construction in normal use.
type RootKeyring struct {
	keys map[string]ed25519.PublicKey
}

// NewRootKeyring builds an empty keyring.
func NewRootKeyring() *RootKeyring { return &RootKeyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers an authorized public key under keyID. A later Add for the same id
// replaces the key, which is a key rotation. It refuses an empty id or a malformed
// key so the ring never holds one that can never verify.
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

// VerifyCheckpoint verifies a COSE_Sign1 signed checkpoint against the keyring and
// returns the checkpoint it attests. It fails closed: an unknown key, a wrong
// content type, a bad signature, an algorithm other than the expected one, or a
// payload that does not decode are all rejected. The returned checkpoint comes from
// the signed payload, never from any unsigned carried copy.
func VerifyCheckpoint(coseBytes []byte, ring *RootKeyring) (Checkpoint, error) {
	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(coseBytes); err != nil {
		return Checkpoint{}, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	if ct, _ := msg.Headers.Protected[cose.HeaderLabelContentType].(string); ct != checkpointContentType {
		return Checkpoint{}, fault.New(fault.Terminal, CodeContentType, "chain: unexpected checkpoint content type")
	}
	kid, _ := msg.Headers.Protected[cose.HeaderLabelKeyID].([]byte)
	pub, ok := ring.keys[string(kid)]
	if !ok {
		return Checkpoint{}, fault.New(fault.Terminal, CodeUnknownKey, "chain: checkpoint signed by an unknown key")
	}
	verifier, err := cose.NewVerifier(checkpointAlg, pub)
	if err != nil {
		return Checkpoint{}, fault.Wrap(fault.Terminal, CodeSignerKey, err)
	}
	if err := msg.Verify(nil, verifier); err != nil {
		return Checkpoint{}, fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	return decodeCheckpoint(msg.Payload)
}
