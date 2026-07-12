package notices

import (
	"crypto/ed25519"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// The signed envelope. EdDSA over Ed25519 is the instance identity key type Flynn
// already signs records with, so a notice feed needs no second key format and no second
// verifier. The content type goes in the protected header, which means it is covered by
// the signature: a valid signature over some other Flynn document can never be presented
// as a valid signature over a notice feed.
const (
	feedAlg         = cose.AlgorithmEdDSA
	feedContentType = "application/flynn-notices+cbor"
)

// Signer produces a signed feed. Publishing side only: the flynn binary never signs a
// feed, it only verifies one, and the private key never comes near a user's machine.
type Signer struct {
	keyID  string
	signer cose.Signer
}

// NewSigner builds a signer over an Ed25519 private key. An empty key id is refused
// because a feed nobody can attribute to a key is a feed nobody can revoke.
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

// Sign encodes f deterministically and returns the COSE_Sign1 document to publish. The
// algorithm, content type, and key id are all in the protected header and so are all
// covered by the signature. Ed25519 signing is deterministic, so this needs no
// randomness.
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

// Keyring is the set of public keys allowed to sign a notice feed. A key that is not in
// the ring cannot say anything to this binary, so revoking a publisher is removing its
// key and shipping a build. The ring is compiled in (see keyring.go) rather than
// configurable: a keyring a user can add to is a keyring an attacker who can write one
// file can add to, and this is the one channel that must survive that.
type Keyring struct {
	keys map[string]ed25519.PublicKey
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring { return &Keyring{keys: map[string]ed25519.PublicKey{}} }

// Add registers pub under keyID. Adding the same id again replaces the key, which is how
// a rotation is expressed.
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

// Len reports how many keys the ring holds. A ring with no keys can verify nothing, and
// a client that finds itself holding one says so rather than silently never showing a
// notice again.
func (k *Keyring) Len() int { return len(k.keys) }

// Verify checks a signed feed document against the keyring and returns the feed it
// attests. It fails closed on every axis: an unknown key id, a content type that is not
// ours, a signature that does not verify, an algorithm other than EdDSA, a payload that
// is not canonical CBOR, or a payload that does not pass structural validation. Nothing
// partial is ever returned, because a half-trusted advisory is worse than none: it would
// be rendered.
//
// Verify does not check the feed's freshness. That is Accept's job, because freshness
// needs the client's trust state and clock, and keeping the two apart means the pure
// crypto here can be property-tested without either.
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
	// The feed returned is decoded from the signed payload and from nothing else. There
	// is no unsigned carried copy to be tempted by.
	return decodePayload(msg.Payload)
}
