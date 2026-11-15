package controlplane

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

// Identity is an instance's Ed25519 keypair: its identity on the fleet and the key it
// signs delegations with, checkable by another instance with no central authority.
type Identity struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// GenerateIdentity creates a fresh in-memory identity; [LoadOrCreateIdentity]
// persists one across restarts.
func GenerateIdentity() (*Identity, error) {
	// Entropy comes from package ids, the single sanctioned randomness source. A 256-bit
	// token is exactly an Ed25519 seed, and the key derives deterministically from it.
	tok, err := ids.Token()
	if err != nil {
		return nil, fmt.Errorf("controlplane: generate identity: %w", err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("controlplane: generate identity: malformed entropy")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("controlplane: generate identity: unexpected key type")
	}
	return &Identity{pub: pub, priv: priv}, nil
}

// Public returns the identity's public key, the value other instances verify its
// delegations against.
func (i *Identity) Public() ed25519.PublicKey { return i.pub }

// ID returns the principal id derived from the public key, so the id is
// self-certifying.
func (i *Identity) ID() string { return PrincipalID(i.pub) }

// PrincipalID renders an Ed25519 public key as a stable, self-certifying principal id.
func PrincipalID(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(pub)
}

// Authority is what a verified token confers: the leaf subject, the minimum scope
// across the chain, and the intersection of every block's grant.
type Authority struct {
	Subject string
	Scope   Scope
	Grant   capability.Grant
}

// capBlock is one link in a delegation chain. The issuer signs the root; each later
// block is signed by the audience the previous block named. A block grants its
// audience every action (AllowAll, root only) or exactly the listed actions, up to the
// scope, until NotAfter.
type capBlock struct {
	Scope    Scope    `json:"scope"`
	AllowAll bool     `json:"allowAll,omitempty"`
	Actions  []string `json:"actions,omitempty"`
	Audience []byte   `json:"aud"`
	NotAfter int64    `json:"notAfter"`
}

func (b capBlock) grant() capability.Grant {
	if b.AllowAll {
		return capability.AllowAll()
	}
	return capability.NewGrant(b.Actions...)
}

// sealedBlock keeps a capBlock's signed bytes verbatim beside the signature, so
// verification never depends on a re-serialization matching.
type sealedBlock struct {
	Payload []byte `json:"p"`
	Sig     []byte `json:"s"`
}

// capToken is the wire form: the ordered chain of sealed blocks.
type capToken struct {
	Blocks []sealedBlock `json:"b"`
}

// Issue mints a root token: issuer signs scope and grant to audience (the key allowed
// to use or attenuate it), valid until notAfter. It verifies offline against the
// issuer's public key.
func Issue(issuer *Identity, audience ed25519.PublicKey, scope Scope, grant capability.Grant, notAfter time.Time) (string, error) {
	if issuer == nil {
		return "", errors.New("controlplane: issue: nil issuer")
	}
	if len(audience) != ed25519.PublicKeySize {
		return "", errors.New("controlplane: issue: invalid audience key")
	}
	blk := capBlock{
		Scope:    scope,
		AllowAll: grant.Unrestricted(),
		Actions:  grant.Actions(),
		Audience: append([]byte(nil), audience...),
		NotAfter: notAfter.Unix(),
	}
	sb, err := sign(blk, issuer.priv)
	if err != nil {
		return "", err
	}
	return encodeToken(capToken{Blocks: []sealedBlock{sb}})
}

// Attenuate appends a narrowing block. holder must be the token's current leaf
// audience; the block names a scope, an explicit action subset and nextAudience.
// Verify rejects any block that widens authority.
func Attenuate(tok string, holder *Identity, nextAudience ed25519.PublicKey, scope Scope, actions []string, notAfter time.Time) (string, error) {
	if holder == nil {
		return "", errors.New("controlplane: attenuate: nil holder")
	}
	if len(nextAudience) != ed25519.PublicKeySize {
		return "", errors.New("controlplane: attenuate: invalid audience key")
	}
	t, err := decodeToken(tok)
	if err != nil {
		return "", err
	}
	if len(t.Blocks) == 0 {
		return "", errors.New("controlplane: attenuate: empty token")
	}
	leaf, err := parseBlock(t.Blocks[len(t.Blocks)-1].Payload)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(leaf.Audience, holder.pub) {
		return "", errors.New("controlplane: attenuate: holder is not the token's current audience")
	}
	blk := capBlock{
		Scope:    scope,
		AllowAll: false, // an attenuation always carries an explicit, narrowed action set
		Actions:  sortedUnique(actions),
		Audience: append([]byte(nil), nextAudience...),
		NotAfter: notAfter.Unix(),
	}
	sb, err := sign(blk, holder.priv)
	if err != nil {
		return "", err
	}
	t.Blocks = append(t.Blocks, sb)
	return encodeToken(t)
}

// Verify checks tok against the trusted issuer key as of now and returns the authority
// it confers. It fails closed on a broken signature, a widening block, an expired
// block or a malformed token, so a token can only have been narrowed in transit. A
// token is a bearer credential: Verify does not check who presents it, which
// [RequirePossession] adds.
func Verify(tok string, issuer ed25519.PublicKey, now time.Time) (Authority, error) {
	t, err := decodeToken(tok)
	if err != nil {
		return Authority{}, err
	}
	if len(t.Blocks) == 0 {
		return Authority{}, errors.New("controlplane: verify: empty token")
	}

	// The root block must be signed by the trusted issuer; it sets the ceiling.
	root, err := verifyBlock(t.Blocks[0], issuer, now)
	if err != nil {
		return Authority{}, fmt.Errorf("controlplane: verify root: %w", err)
	}
	effScope := root.Scope
	effGrant := root.grant()
	prevAud := root.Audience

	// Each later block must be signed by the previous block's audience and may only
	// narrow the running authority.
	for idx := 1; idx < len(t.Blocks); idx++ {
		blk, err := verifyBlock(t.Blocks[idx], prevAud, now)
		if err != nil {
			return Authority{}, fmt.Errorf("controlplane: verify block %d: %w", idx, err)
		}
		if blk.AllowAll {
			return Authority{}, fmt.Errorf("controlplane: verify block %d: an attenuation may not be unrestricted", idx)
		}
		if blk.Scope > effScope {
			return Authority{}, fmt.Errorf("controlplane: verify block %d: scope escalation", idx)
		}
		for _, a := range blk.Actions {
			if !effGrant.Allows(a) {
				return Authority{}, fmt.Errorf("controlplane: verify block %d: action %q exceeds the delegated grant", idx, a)
			}
		}
		effScope = blk.Scope
		effGrant = effGrant.Narrow(blk.Actions...)
		prevAud = blk.Audience
	}
	return Authority{Subject: PrincipalID(prevAud), Scope: effScope, Grant: effGrant}, nil
}

// DelegationAuthenticator is the [Authenticator] for capability tokens: it verifies a
// bearer token offline against the trusted issuer key and turns the proven authority
// into a [Principal]. An unverifiable token is unauthenticated, so no authority beyond
// what the token carried can come out of it.
//
//flynn:policy TestFailClosedAuthenticators
type DelegationAuthenticator struct {
	issuer ed25519.PublicKey
	clk    clock.Clock
}

// NewDelegationAuthenticator accepts tokens issued, directly or by delegation, under
// issuer, checking expiry against clk (nil means the system clock). An issuer key of
// the wrong size makes every token fail.
func NewDelegationAuthenticator(issuer ed25519.PublicKey, clk clock.Clock) *DelegationAuthenticator {
	if clk == nil {
		clk = clock.System{}
	}
	return &DelegationAuthenticator{issuer: issuer, clk: clk}
}

// Authenticate resolves the bearer token to a Principal carrying the verified Grant,
// or returns ErrUnauthenticated.
func (a *DelegationAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	tok := bearerToken(r)
	if tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	auth, err := Verify(tok, a.issuer, a.clk.Now())
	if err != nil {
		// Every verification failure looks like no credential, so a probing client cannot
		// tell a tampered chain from an expired one or an unknown issuer.
		return Principal{}, ErrUnauthenticated
	}
	return Principal{ID: auth.Subject, Scope: auth.Scope, Grant: auth.Grant}, nil
}

// verifyBlock checks one block's signature against the signer that must have produced it
// and that it is well formed and unexpired, returning the decoded block.
func verifyBlock(sb sealedBlock, signer ed25519.PublicKey, now time.Time) (capBlock, error) {
	if len(signer) != ed25519.PublicKeySize {
		return capBlock{}, errors.New("invalid signer key")
	}
	if !ed25519.Verify(signer, sb.Payload, sb.Sig) {
		return capBlock{}, errors.New("bad signature")
	}
	blk, err := parseBlock(sb.Payload)
	if err != nil {
		return capBlock{}, err
	}
	if len(blk.Audience) != ed25519.PublicKeySize {
		return capBlock{}, errors.New("malformed audience")
	}
	if now.Unix() > blk.NotAfter {
		return capBlock{}, errors.New("expired")
	}
	return blk, nil
}

func sign(blk capBlock, priv ed25519.PrivateKey) (sealedBlock, error) {
	payload, err := json.Marshal(blk)
	if err != nil {
		return sealedBlock{}, fmt.Errorf("controlplane: marshal block: %w", err)
	}
	return sealedBlock{Payload: payload, Sig: ed25519.Sign(priv, payload)}, nil
}

func parseBlock(payload []byte) (capBlock, error) {
	var blk capBlock
	if err := json.Unmarshal(payload, &blk); err != nil {
		return capBlock{}, fmt.Errorf("malformed block: %w", err)
	}
	return blk, nil
}

func encodeToken(t capToken) (string, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("controlplane: encode token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeToken(s string) (capToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return capToken{}, fmt.Errorf("controlplane: decode token: %w", err)
	}
	var t capToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return capToken{}, fmt.Errorf("controlplane: parse token: %w", err)
	}
	return t, nil
}

func sortedUnique(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, a := range in {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
