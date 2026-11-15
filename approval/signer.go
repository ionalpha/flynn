package approval

import (
	"crypto/ed25519"
	"io"

	"github.com/ionalpha/flynn/fault"
)

// Signer produces a detached signature over an [Envelope]. The gate verifies
// signatures without depending on how they were produced, so Ed25519, a
// hardware token, a KMS or a wallet can sit behind it.
type Signer interface {
	// Sign returns an Approval carrying e, the KeyID and the signature over e's
	// signing bytes.
	Sign(e Envelope) (Approval, error)
	// KeyID names the signing key in a verifier's keyring.
	KeyID() string
}

// Ed25519Signer is the default signer, from the standard library.
type Ed25519Signer struct {
	keyID string
	priv  ed25519.PrivateKey
}

// NewEd25519Signer builds a signer over priv. It refuses a malformed key so a
// signer can never be unable to sign.
func NewEd25519Signer(keyID string, priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if keyID == "" {
		return nil, fault.New(fault.Terminal, "approval_empty_key_id", "approval: signer key id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, "approval_bad_private_key", "approval: malformed Ed25519 private key")
	}
	return &Ed25519Signer{keyID: keyID, priv: priv}, nil
}

// GenerateEd25519Signer mints a keypair from rand and returns the signer and
// the public key to register in a keyring. Production passes crypto/rand.Reader;
// a test can pass a deterministic reader.
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

// KeyID names the key a [Keyring] verifies this signer's approvals under.
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

// Keyring maps key id to the public key of an authorized approver. Only a
// signature from a key in the ring counts, so revoking an approver means
// removing their key. It is read-only after construction, so concurrent reads
// are safe.
//
//flynn:policy TestFailClosedVerifier
type Keyring struct {
	keys map[string]ed25519.PublicKey
}

// NewKeyring returns a keyring that trusts no key.
func NewKeyring() *Keyring { return &Keyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers pub under keyID, replacing any key already there (a rotation).
// It refuses a malformed key, which could never verify.
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

// verify reports whether sig is a valid signature over msg by keyID. An unknown
// keyID never verifies, so an unauthorized key is rejected like a forgery.
func (k *Keyring) verify(keyID string, msg, sig []byte) bool {
	pub, ok := k.keys[keyID]
	if !ok {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
