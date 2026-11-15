package notices

import (
	"crypto/ed25519"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// Ed25519 is the key type Flynn already signs records with, so the feed needs no
// second verifier. The content type is in the protected header, so a signature over
// another Flynn document cannot pass as one over a feed.
const (
	feedAlg         = cose.AlgorithmEdDSA
	feedContentType = "application/flynn-notices+cbor"
)

// Signer signs a feed. Only the publisher uses it; the flynn binary only verifies.
type Signer struct {
	keyID  string
	signer cose.Signer
}

// NewSigner refuses an empty key id, because a feed with no key id cannot be revoked.
func NewSigner(keyID string, priv ed25519.PrivateKey) (*Signer, error) {
	if keyID == "" {
		return nil, fault.New(fault.Terminal, CodeSignerKey, "notices: signer key id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fault.New(fault.Terminal, CodeSignerKey, "notices: malformed Ed25519 private key")
	}
	s, err := cose.NewSigner(feedAlg, priv)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSignerKey, err)
	}
	return &Signer{keyID: keyID, signer: s}, nil
}

// KeyID identifies the signing key in a client's keyring.
func (s *Signer) KeyID() string { return s.keyID }

// Sign returns the COSE_Sign1 document for f. Algorithm, content type and key id are
// in the protected header, covered by the signature.
func (s *Signer) Sign(f Feed) ([]byte, error) {
	pay, err := payload(f)
	if err != nil {
		return nil, err
	}
	headers := cose.Headers{
		Protected: cose.ProtectedHeader{
			cose.HeaderLabelAlgorithm:   feedAlg,
			cose.HeaderLabelContentType: feedContentType,
			cose.HeaderLabelKeyID:       []byte(s.keyID),
		},
	}
	msg, err := cose.Sign1(nil, s.signer, headers, pay, nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeSignature, err)
	}
	return msg, nil
}

// Keyring is the set of public keys allowed to sign a feed. The production ring is
// compiled in ([DefaultKeyring]) and not configurable, because an attacker who can
// write one file could add a key to a configurable ring.
type Keyring struct {
	keys map[string]ed25519.PublicKey
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring { return &Keyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers pub under keyID, replacing any key already there.
func (k *Keyring) Add(keyID string, pub ed25519.PublicKey) error {
	if keyID == "" {
		return fault.New(fault.Terminal, CodeSignerKey, "notices: keyring id must not be empty")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fault.New(fault.Terminal, CodeSignerKey, "notices: malformed Ed25519 public key for "+keyID)
	}
	k.keys[keyID] = pub
	return nil
}

// Len reports how many keys the ring holds. An empty ring verifies nothing.
func (k *Keyring) Len() int { return len(k.keys) }

// Verify checks doc against ring and returns the feed it attests. It fails on an
// unknown key id, a foreign content type, a bad signature, a non-EdDSA algorithm, or
// a payload that is not canonical or not well formed, and never returns a partial feed.
//
// Freshness is [Accept]'s job, so Verify needs no trust state or clock and can be
// property-tested alone.
func Verify(doc []byte, ring *Keyring) (Feed, error) {
	if len(doc) == 0 {
		return Feed{}, fault.New(fault.Terminal, CodeDecode, "notices: empty feed document")
	}
	if len(doc) > MaxFeedBytes {
		return Feed{}, fault.New(fault.Terminal, CodeTooLarge, "notices: feed document is too large")
	}
	if ring == nil || ring.Len() == 0 {
		return Feed{}, fault.New(fault.Terminal, CodeUnknownKey, "notices: no keys to verify a feed against")
	}

	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(doc); err != nil {
		return Feed{}, fault.Wrap(fault.Terminal, CodeSignature, err)
	}
	if ct, _ := msg.Headers.Protected[cose.HeaderLabelContentType].(string); ct != feedContentType {
		return Feed{}, fault.New(fault.Terminal, CodeContentType, "notices: unexpected feed content type")
	}
	kid, _ := msg.Headers.Protected[cose.HeaderLabelKeyID].([]byte)
	pub, ok := ring.keys[string(kid)]
	if !ok {
		return Feed{}, fault.New(fault.Terminal, CodeUnknownKey, "notices: feed signed by an unknown key")
	}
	verifier, err := cose.NewVerifier(feedAlg, pub)
	if err != nil {
		return Feed{}, fault.Wrap(fault.Terminal, CodeSignerKey, err)
	}
	if err := msg.Verify(nil, verifier); err != nil {
		return Feed{}, fault.Wrap(fault.Terminal, CodeSignature, err)
	}
	// Decoded from the signed payload only.
	return decodePayload(msg.Payload)
}
