package controlplane

// This file is the cryptographic core of cross-instance authority: a public-key,
// offline-verifiable, attenuable capability token. An issuer mints a token over a
// capability grant; a holder may attenuate it (only ever narrowing the authority) and
// delegate it onward; any party holding the issuer's public key can verify a token and
// extract its effective authority, and can forge nothing. This is what makes "no
// authority escalation across the wire" a cryptographic property rather than a matter
// of trust: a token can only have been narrowed in transit, never widened.
//
// The implementation is a minimal own-Ed25519 scheme with no third-party dependency, so
// the open binary carries it with zero infrastructure. It sits behind no interface here
// yet by design, but its shape (issue, attenuate, verify over a capability.Grant) is the
// port a Datalog-checked token library can later implement without changing callers.
//
// Scope and threat model of THIS layer: the token is unforgeable and monotonically
// narrowing, and verification fails closed on any broken signature, widened block,
// expired block, or malformed token. It is a bearer credential: binding a token to its
// holder so a stolen or truncated token cannot be replayed is proof-of-possession, a
// separate hardening layer that also seals the chain length. Verify therefore returns
// the authority a presented chain encodes; pinning who may present it is layered on top.

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

// Identity is an instance's self-issued Ed25519 keypair: its verifiable identity on the
// fleet and the key it signs delegations with. It extends the bookkeeping OriginInstanceID
// into something another instance can cryptographically check, with no central authority.
type Identity struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// GenerateIdentity creates a fresh instance identity. Persisting the key across restarts
// (sealed in the vault) is a later hardening step; a generated-in-memory identity is
// enough to issue and verify within a process and across a fleet that exchanges public
// keys.
func GenerateIdentity() (*Identity, error) {
	// Entropy comes from the ids package (the one sanctioned randomness source), not
	// crypto/rand directly, so the project's single-entropy-boundary rule holds. A
	// 256-bit token is exactly an Ed25519 seed; the key derives deterministically from
	// it, so a future deterministic entropy source reproduces the identity for replay.
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

// ID returns the identity's stable principal id, derived from its public key so it is
// self-certifying: the id and the verifying key are one and the same.
func (i *Identity) ID() string { return PrincipalID(i.pub) }

// PrincipalID renders an Ed25519 public key as a stable, self-certifying principal id.
func PrincipalID(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(pub)
}

// Authority is the effective permission a verified token confers: the leaf subject it
// was delegated to, the scope, and the capability grant (the intersection of every
// block in the chain). It is the value a request gate checks an action against.
type Authority struct {
	Subject string
	Scope   Scope
	Grant   capability.Grant
}

// capBlock is one link in a delegation chain. The root block is signed by the issuer;
// each later block attenuates the authority and is signed by the key the previous block
// named as audience, so the chain is a verifiable delegation path. A block grants its
// audience either every action (AllowAll, root only) or exactly the listed actions, up
// to the scope, until NotAfter.
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

// sealedBlock is a capBlock together with the signature over its exact serialized bytes.
// The signed bytes are stored verbatim so verification checks the signature over the
// same bytes that were signed, never a re-serialization that could differ.
type sealedBlock struct {
	Payload []byte `json:"p"`
	Sig     []byte `json:"s"`
}

// capToken is the wire form: the ordered chain of sealed blocks.
type capToken struct {
	Blocks []sealedBlock `json:"b"`
}

// Issue mints a root capability token: the issuer signs scope and grant to an audience
// public key (the holder authorized to exercise or further attenuate it), valid until
// notAfter. The token verifies offline against the issuer's public key.
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

// Attenuate appends a narrowing block to a token. The holder must be the token's current
// leaf audience (it signs with the key that block named), and the new block restricts the
// scope and names an explicit action subset and the next audience. Attenuation can only
// shrink authority; Verify rejects any block that tries to widen it. The result is a
// longer chain delegating a strictly smaller authority to nextAudience.
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

// Verify checks a token against the trusted issuer key as of now and returns the
// effective authority it confers: the leaf subject, the minimum scope across the chain,
// and the intersection of every block's grant. It fails closed on any broken signature,
// any block that tries to widen authority (a higher scope or an action the parent did
// not hold), an expired block, or a malformed token, so a token can never confer more
// than the issuer delegated.
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

// DelegationAuthenticator resolves a presented capability token to a Principal,
// implementing the Authenticator boundary the server already gates every request
// through. It is the bridge from the cryptographic delegation layer to the request
// model: a bearer token is verified offline against the trusted issuer key, and the
// authority it proves (subject, scope, and the monotonically attenuated Grant)
// becomes the Principal the action gate later checks. Because Verify fails closed on
// any forged, widened, expired, or malformed chain, an unverifiable token is simply
// unauthenticated; nothing here can mint authority the token did not already carry.
type DelegationAuthenticator struct {
	issuer ed25519.PublicKey
	clk    clock.Clock
}

// NewDelegationAuthenticator builds an authenticator that accepts tokens issued (and
// transitively delegated) under issuer, reading the current time from clk so expiry
// is checked against the same clock the rest of the system uses (System in
// production, Manual in tests). A nil clock falls back to the system clock; an issuer
// of the wrong size makes every token fail to verify, which is the fail-closed
// outcome.
func NewDelegationAuthenticator(issuer ed25519.PublicKey, clk clock.Clock) *DelegationAuthenticator {
	if clk == nil {
		clk = clock.System{}
	}
	return &DelegationAuthenticator{issuer: issuer, clk: clk}
}

// Authenticate verifies the request's bearer token and resolves it to a Principal
// carrying the verified Grant. A missing or unverifiable token is ErrUnauthenticated,
// never a partial or escalated authority.
func (a *DelegationAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	tok := bearerToken(r)
	if tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	auth, err := Verify(tok, a.issuer, a.clk.Now())
	if err != nil {
		// A token that does not verify is indistinguishable from no credential: the
		// caller learns only that it was refused, not why, so a probing client cannot
		// tell a tampered chain from an expired one from an unknown issuer.
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
