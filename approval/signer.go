package approval

import (
	"crypto/ed25519"
	"io"

	"github.com/ionalpha/flynn/fault"
)

// Signer produces a detached signature over an Envelope. It is the approver's
// side of the boundary: the method (Ed25519, a hardware token, a KMS, a wallet)
// is an implementation detail behind this port, so the gate verifies a signature
// without depending on how it was produced. KeyID identifies the signing key so a
// verifier knows which public key to check against and the audit trail shows who
// signed.
type Signer interface {
	// Sign returns an Approval carrying e, the signer's KeyID, and the signature
	// over e's signing bytes.
	Sign(e Envelope) (Approval, error)
	// KeyID identifies the signing key in a verifier's keyring.
	KeyID() string
}

// Ed25519Signer signs envelopes with an Ed25519 private key. It is the default,
// standard-library signer: no external dependency, small keys, fast verification.
type Ed25519Signer struct {
	keyID string
	priv  ed25519.PrivateKey
}

// NewEd25519Signer builds a signer over an existing private key, identified by
// keyID. It refuses a malformed key so a signer is never silently unable to sign.
func NewEd25519Signer(keyID string, priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if keyID == "" {
		return nil, fault.New(fault.Terminal, "approval_empty_key_id", "approval: signer key id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, "approval_bad_private_key", "approval: malformed Ed25519 private key")
	}
	return &Ed25519Signer{keyID: keyID, priv: priv}, nil
}

// GenerateEd25519Signer mints a fresh Ed25519 keypair from rand and returns a
// signer over it together with its public key, so a caller can register the public
// key in a verifier's keyring. In production rand is crypto/rand.Reader; a test can
// inject a deterministic reader.
func GenerateEd25519Signer(keyID string, rand io.Reader) (*Ed25519Signer, ed25519.PublicKey, error) {
	if keyID == "" {
		return nil, nil, fault.New(fault.Terminal, "approval_empty_key_id", "approval: signer key id must not be empty")
	}
	pub, priv, err := ed25519.GenerateKey(rand)
	if err != nil {
		return nil, nil, fault.Wrap(fault.Terminal, "approval_keygen", err)
	}
	return &Ed25519Signer{keyID: keyID, priv: priv}, pub, nil
}

// KeyID identifies the signing key.
func (s *Ed25519Signer) KeyID() string { return s.keyID }

// Sign signs e and returns the Approval.
func (s *Ed25519Signer) Sign(e Envelope) (Approval, error) {
	msg, err := e.signingBytes()
	if err != nil {
		return Approval{}, fault.Wrap(fault.Terminal, "approval_sign_encode", err)
	}
	return Approval{Envelope: e, KeyID: s.keyID, Signature: ed25519.Sign(s.priv, msg)}, nil
}

var _ Signer = (*Ed25519Signer)(nil)

// Keyring is the set of authorized approvers: a map from key id to the public key
// that authorizes signatures from it. Only a signature from a key in the ring can
// count toward an authorization, so revoking an approver is removing their key.
// It is read-only after construction in normal use, so it is safe for concurrent
// reads.
type Keyring struct {
	keys map[string]ed25519.PublicKey
}

// NewKeyring builds an empty keyring.
func NewKeyring() *Keyring { return &Keyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers an authorized approver's public key under keyID. A later Add for
// the same id replaces the key (a key rotation). It refuses a malformed key so the
// ring never holds one that can never verify.
func (k *Keyring) Add(keyID string, pub ed25519.PublicKey) error {
	if keyID == "" {
		return fault.New(fault.Terminal, "approval_empty_key_id", "approval: keyring id must not be empty")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fault.New(fault.Terminal, "approval_bad_public_key", "approval: malformed Ed25519 public key for "+keyID)
	}
	k.keys[keyID] = pub
	return nil
}

// verify reports whether sig is a valid signature over msg by the key registered
// under keyID. An unknown keyID never verifies, so a signature from an
// unauthorized key is rejected exactly like a forged one.
func (k *Keyring) verify(keyID string, msg, sig []byte) bool {
	pub, ok := k.keys[keyID]
	if !ok {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
