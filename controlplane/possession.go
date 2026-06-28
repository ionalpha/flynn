package controlplane

// Proof-of-possession closes the bearer-token theft-replay gap. The keystone token is
// unforgeable and monotonically narrowing, but it is still a bearer credential: whoever
// holds the bytes can present them. If a token leaks (a log, a proxy, a stolen backup),
// the thief replays it. This layer binds use of a token to possession of the leaf
// private key the token names as its audience, so a stolen token without the matching
// key is inert.
//
// Two interchangeable possession proofs sit behind one identity, both proving the caller
// holds the leaf key without ever transmitting it:
//
//   - A signed proof header. The holder signs a small claim (the principal id it asserts,
//     the request method and path, a timestamp, and a single-use nonce) with the leaf
//     private key. The server verifies the signature with the leaf public key carried in
//     the verified token, checks the claim is fresh and bound to this exact request, and
//     refuses a replayed nonce. Channel-independent: works over plain HTTP behind a TLS
//     terminator that the application cannot see.
//
//   - mTLS channel binding. When the request arrives over a TLS connection whose verified
//     client certificate carries the leaf public key, possession is proven by the TLS
//     handshake itself and no signed header is needed. This is the locked spec's "mTLS as
//     an option": the same identity, proven at the transport instead of in a header.
//
// PossessionAuthenticator decorates any inner Authenticator (the delegation token
// authenticator in production): the inner step proves what authority the token carries,
// this step proves the presenter holds the key it was issued to. Either proof failing is
// indistinguishable from no credential, so a probing caller learns nothing.

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

// defaultProofSkew is how far a proof's timestamp may sit from the server's clock and
// still be accepted. It bounds both clock drift and the window a captured proof could be
// replayed against a different endpoint; the single-use nonce closes replay against the
// same endpoint within that window.
const defaultProofSkew = 30 * time.Second

// ParsePrincipalID decodes a principal id minted by PrincipalID back into its Ed25519
// public key. It is the inverse of PrincipalID, used to recover the leaf key a verified
// token was issued to so a possession proof can be checked against it. A malformed id or
// a key of the wrong length fails closed.
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

// proofClaims is the signed assertion of possession. Binding the method and path stops a
// proof captured on one request from being replayed against another; the nonce makes a
// proof single-use; the timestamp bounds how long a captured proof is even a candidate
// for replay.
type proofClaims struct {
	Sub    string `json:"sub"`
	Method string `json:"mth"`
	Path   string `json:"pth"`
	Issued int64  `json:"iat"`
	Nonce  string `json:"nce"`
}

// SignProof builds the signed possession proof a holder presents alongside its token.
// The holder signs that it possesses the leaf key (identified by holder.ID()) and binds
// the proof to this exact request (method, path) at this instant, with a single-use
// nonce. The returned value is placed in the ProofHeader. The nonce must be unique per
// request (ids.Token in production); reusing one is refused by the server as a replay.
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

// PossessionAuthenticator wraps an inner Authenticator and additionally requires the
// caller to prove possession of the leaf key the resolved principal is bound to. The
// inner authenticator establishes authority (the verified, attenuated Grant); this layer
// establishes that the presenter actually holds the key, not just a copy of the token.
//
// A request is admitted only if the inner authenticator resolves a principal AND a valid
// possession proof binds to that principal: either a signed ProofHeader, or, when
// enabled, an mTLS client certificate carrying the leaf key. Any failure collapses to
// ErrUnauthenticated.
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

// WithTLSPossession enables mTLS channel binding as an accepted possession proof: a
// request over a TLS connection whose verified client certificate carries the leaf
// public key is admitted without a signed header. Off by default, since it requires the
// server to be configured to request and verify client certificates.
func WithTLSPossession() PossessionOption {
	return func(p *PossessionAuthenticator) { p.allowTLS = true }
}

// RequirePossession wraps inner so that, beyond resolving a principal, every request must
// prove possession of the principal's leaf key. clk supplies the time proofs are checked
// against (System in production, Manual in tests); a nil clock falls back to the system
// clock.
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

// Authenticate resolves the request through the inner authenticator, then requires a
// valid possession proof for the resolved principal. A missing or invalid proof, or a
// proof that does not bind to the resolved principal, is ErrUnauthenticated, so a stolen
// token alone never authenticates.
func (p *PossessionAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	prin, err := p.inner.Authenticate(r)
	if err != nil {
		return Principal{}, err
	}
	leaf, err := ParsePrincipalID(prin.ID)
	if err != nil {
		// The principal's id is not a key we can demand possession of. Rather than
		// admit it unproven, refuse: this layer is only mounted where principals are
		// key-identified, so a non-key id here is a misconfiguration that must fail
		// closed, not silently skip the possession check.
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

// tlsProvesPossession reports whether the request arrived over TLS with a verified client
// certificate whose public key is the leaf key. crypto/tls only populates
// VerifiedChains when the server was configured to require and verify client certs, so a
// certificate here has already been validated against the configured CA; we additionally
// require its key to be the exact leaf the token names.
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

// nonceCache records used proof nonces until they expire, so a captured proof cannot be
// replayed even inside the freshness window. Each nonce is kept only until its proof
// would have gone stale anyway (issue time + skew), so the cache is self-bounding: it
// never needs to hold a nonce longer than a proof could be valid. It is safe for
// concurrent use.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{seen: make(map[string]time.Time)}
}

// use records nonce as spent and reports whether it was previously unused. A return of
// false means the nonce was already spent (a replay). The nonce is retained until now+ttl
// (past which any proof bearing it is stale anyway and rejected before reaching here), and
// expired entries are evicted on each call, so the map self-bounds to only live nonces.
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
