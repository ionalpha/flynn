package controlplane

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// ProofHeader is the request header carrying the signed possession proof.
const ProofHeader = "Flynn-Proof"

// defaultProofSkew is how far a proof's timestamp may differ from the server clock. It
// bounds clock drift and how long a captured proof could be replayed against another
// endpoint; the nonce stops replay against the same one.
const defaultProofSkew = 30 * time.Second

// ParsePrincipalID is the inverse of [PrincipalID]: it recovers the leaf key a
// verified token names, so a possession proof can be checked. A malformed id or a key
// of the wrong length fails.
func ParsePrincipalID(id string) (ed25519.PublicKey, error) {
	const prefix = "ed25519:"
	if len(id) <= len(prefix) || id[:len(prefix)] != prefix {
		return nil, errors.New("controlplane: principal id is not an ed25519 key")
	}
	raw, err := base64.RawURLEncoding.DecodeString(id[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("controlplane: decode principal id: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("controlplane: principal id key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// proofClaims is the signed assertion of possession. Method and path stop a proof
// captured on one request being replayed on another, the nonce makes it single-use,
// and the timestamp bounds how long it is a replay candidate at all.
type proofClaims struct {
	Sub    string `json:"sub"`
	Method string `json:"mth"`
	Path   string `json:"pth"`
	Issued int64  `json:"iat"`
	Nonce  string `json:"nce"`
}

// SignProof builds the proof a holder sends in [ProofHeader]: a signature by holder's
// key binding method, path, now and nonce. nonce must be unique per request
// (ids.Token in production); the server refuses a reused one.
func SignProof(holder *Identity, method, path, nonce string, now time.Time) (string, error) {
	if holder == nil {
		return "", errors.New("controlplane: sign proof: nil holder")
	}
	if nonce == "" {
		return "", errors.New("controlplane: sign proof: empty nonce")
	}
	claims := proofClaims{
		Sub:    holder.ID(),
		Method: method,
		Path:   path,
		Issued: now.Unix(),
		Nonce:  nonce,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("controlplane: marshal proof: %w", err)
	}
	sig := ed25519.Sign(holder.priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// PossessionAuthenticator wraps an inner [Authenticator] and also requires proof that
// the caller holds the leaf private key the token was issued to, so a leaked token
// without the key is useless. The inner step proves authority; this one proves
// possession, without the key ever being sent. Two proofs are accepted:
//
//   - a signed [ProofHeader]: the principal id, request method and path, a timestamp
//     and a single-use nonce, signed with the leaf key. It works behind a TLS
//     terminator the application cannot see.
//   - with [WithTLSPossession], a verified mTLS client certificate carrying the leaf
//     key.
//
// Any failure is ErrUnauthenticated, indistinguishable from no credential.
//
//flynn:policy TestFailClosedAuthenticators
type PossessionAuthenticator struct {
	inner    Authenticator
	clk      clock.Clock
	skew     time.Duration
	allowTLS bool
	nonces   *nonceCache
}

// PossessionOption configures a PossessionAuthenticator.
type PossessionOption func(*PossessionAuthenticator)

// WithProofSkew overrides the accepted timestamp skew for signed proofs. A non-positive
// value is ignored, keeping the safe default.
func WithProofSkew(d time.Duration) PossessionOption {
	return func(p *PossessionAuthenticator) {
		if d > 0 {
			p.skew = d
		}
	}
}

// WithTLSPossession also accepts a verified mTLS client certificate carrying the leaf
// key as proof. It is off by default because the server must be configured to
// request and verify client certificates.
func WithTLSPossession() PossessionOption {
	return func(p *PossessionAuthenticator) { p.allowTLS = true }
}

// RequirePossession wraps inner so every request must also prove possession of the
// principal's leaf key, with proof times checked against clk (nil means the system
// clock).
func RequirePossession(inner Authenticator, clk clock.Clock, opts ...PossessionOption) *PossessionAuthenticator {
	if clk == nil {
		clk = clock.System{}
	}
	p := &PossessionAuthenticator{
		inner:  inner,
		clk:    clk,
		skew:   defaultProofSkew,
		nonces: newNonceCache(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Authenticate resolves the principal through the inner authenticator, then requires
// a possession proof bound to it, so a stolen token alone never authenticates.
func (p *PossessionAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	prin, err := p.inner.Authenticate(r)
	if err != nil {
		return Principal{}, err
	}
	leaf, err := ParsePrincipalID(prin.ID)
	if err != nil {
		// This layer is mounted only where principals are key-identified, so a non-key id is
		// a misconfiguration: refuse rather than skip the check.
		return Principal{}, ErrUnauthenticated
	}
	// mTLS channel binding is accepted first when enabled: if the verified client
	// certificate already carries the leaf key, the handshake itself proved possession.
	if p.allowTLS && tlsProvesPossession(r, leaf) {
		return prin, nil
	}
	if err := p.verifyProofHeader(r, prin.ID, leaf); err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return prin, nil
}

// verifyProofHeader checks the signed proof in ProofHeader: the signature is the leaf
// key's, the claim is bound to this principal and this request, it is fresh, and its
// nonce has not been seen. Marking the nonce seen is the last step, after every other
// check passes, so an invalid proof cannot burn a nonce.
func (p *PossessionAuthenticator) verifyProofHeader(r *http.Request, principalID string, leaf ed25519.PublicKey) error {
	raw := r.Header.Get(ProofHeader)
	if raw == "" {
		return errors.New("missing possession proof")
	}
	payloadB64, sigB64, ok := splitProof(raw)
	if !ok {
		return errors.New("malformed possession proof")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return errors.New("malformed possession proof payload")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return errors.New("malformed possession proof signature")
	}
	if !ed25519.Verify(leaf, payload, sig) {
		return errors.New("possession proof signature does not match the leaf key")
	}
	var claims proofClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("malformed possession proof claims")
	}
	// The proof must assert possession of the same principal the token resolved to, so a
	// valid proof for one key cannot be presented with another key's token.
	if claims.Sub != principalID {
		return errors.New("possession proof subject does not match the principal")
	}
	if claims.Method != r.Method || claims.Path != r.URL.Path {
		return errors.New("possession proof is not bound to this request")
	}
	now := p.clk.Now()
	delta := now.Unix() - claims.Issued
	if delta < 0 {
		delta = -delta
	}
	if time.Duration(delta)*time.Second > p.skew {
		return errors.New("possession proof is stale or postdated")
	}
	if !p.nonces.use(claims.Nonce, now, p.skew) {
		return errors.New("possession proof nonce was already used")
	}
	return nil
}

// splitProof separates the "payload.signature" proof encoding into its two base64
// halves, rejecting any value that is not exactly two non-empty dot-separated parts.
func splitProof(s string) (payload, sig string, ok bool) {
	for i := range len(s) {
		if s[i] == '.' {
			payload, sig = s[:i], s[i+1:]
			// Exactly one separator, both sides present.
			if payload == "" || sig == "" {
				return "", "", false
			}
			for j := i + 1; j < len(s); j++ {
				if s[j] == '.' {
					return "", "", false
				}
			}
			return payload, sig, true
		}
	}
	return "", "", false
}

// tlsProvesPossession reports whether the request came over TLS with a verified
// client certificate whose key is the leaf key. crypto/tls fills VerifiedChains only
// when the server verifies client certificates, so the chain is already checked
// against the configured CA.
func tlsProvesPossession(r *http.Request, leaf ed25519.PublicKey) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	pub, ok := r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return false
	}
	return pub.Equal(leaf)
}

// nonceCache records spent proof nonces so a proof cannot be replayed inside the
// freshness window. A nonce is kept only until its proof would be stale anyway, so
// the cache holds only live nonces. It is safe for concurrent use.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{seen: make(map[string]time.Time)}
}

// use marks nonce spent until now+ttl and reports whether it was unused; false is a
// replay. Expired entries are evicted on each call.
func (c *nonceCache) use(nonce string, now time.Time, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, exp := range c.seen {
		if !exp.After(now) {
			delete(c.seen, n)
		}
	}
	if _, ok := c.seen[nonce]; ok {
		return false
	}
	c.seen[nonce] = now.Add(ttl)
	return true
}
