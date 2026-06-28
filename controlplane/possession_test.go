package controlplane

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
)

// fixedAuth is an inner Authenticator that always resolves the same principal, so a
// possession test exercises the possession layer in isolation from token verification.
type fixedAuth struct {
	prin Principal
	err  error
}

func (f fixedAuth) Authenticate(*http.Request) (Principal, error) { return f.prin, f.err }

// principalFor builds the principal the delegation layer would resolve for a leaf key:
// its id is the leaf's key-derived principal id, which the possession layer recovers the
// public key from.
func principalFor(leaf *Identity) Principal {
	return Principal{ID: leaf.ID(), Scope: ScopeOperator, Grant: capability.AllowAll()}
}

func proofReq(t *testing.T, method, path, proof string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if proof != "" {
		r.Header.Set(ProofHeader, proof)
	}
	return r
}

func TestParsePrincipalIDRoundTrip(t *testing.T) {
	id := dgIdentity(t)
	pub, err := ParsePrincipalID(id.ID())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !pub.Equal(id.Public()) {
		t.Fatal("parsed key differs from the identity's public key")
	}
	for _, bad := range []string{"", "ed25519:", "rsa:abc", "ed25519:!!!", "ed25519:c2hvcnQ"} {
		if _, err := ParsePrincipalID(bad); err == nil {
			t.Fatalf("expected an error for %q", bad)
		}
	}
}

// A request signed by the leaf key is admitted.
func TestPossessionAdmitsValidProof(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)

	proof, err := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	got, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", proof))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != leaf.ID() {
		t.Fatalf("principal id = %q, want %q", got.ID, leaf.ID())
	}
}

// A replayed token with no possession proof is refused: this is the theft-replay gap the
// layer closes.
func TestPossessionRefusesMissingProof(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)

	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", "")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// A proof signed by a different key than the token names (a thief who has the token but
// not the leaf key, and signs with their own key) is refused.
func TestPossessionRefusesWrongKey(t *testing.T) {
	leaf, thief := dgIdentity(t), dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)

	// The thief signs a proof with their own key but tries to pass it with the leaf's
	// token. SignProof stamps the thief's own subject, so even the subject check fails;
	// the signature check would fail regardless.
	proof, err := SignProof(thief, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", proof)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// A proof captured on one request cannot be replayed against a different method or path.
func TestPossessionRefusesCrossRequest(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)

	proof, err := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	if _, err := pa.Authenticate(proofReq(t, http.MethodPost, "/v1/instances", proof)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("method mismatch: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/credentials", proof)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("path mismatch: err = %v, want ErrUnauthenticated", err)
	}
}

// A proof captured and replayed verbatim (same nonce) is refused the second time.
func TestPossessionRefusesReplayedNonce(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)

	proof, err := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", proof)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", proof)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replay: err = %v, want ErrUnauthenticated", err)
	}
}

// A proof whose timestamp is outside the skew window is refused as stale.
func TestPossessionRefusesStaleProof(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk, WithProofSkew(10*time.Second))

	proof, err := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	clk.Advance(11 * time.Second)
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", proof)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// A fresh proof with a new nonce is admitted even after an earlier one expired, proving
// the nonce cache does not wedge the principal out.
func TestPossessionAdmitsFreshAfterExpiry(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk, WithProofSkew(10*time.Second))

	first, _ := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-1", clk.Now())
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", first)); err != nil {
		t.Fatalf("first: %v", err)
	}
	clk.Advance(30 * time.Second)
	second, _ := SignProof(leaf, http.MethodGet, "/v1/instances", "nonce-2", clk.Now())
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/v1/instances", second)); err != nil {
		t.Fatalf("second: %v", err)
	}
}

// An inner authenticator failure short-circuits: the possession layer never runs and the
// inner error propagates.
func TestPossessionPropagatesInnerError(t *testing.T) {
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{err: ErrUnauthenticated}, clk)
	if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/x", "")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// A malformed proof header is refused without panicking.
func TestPossessionRefusesMalformedProof(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk)
	for _, bad := range []string{"no-dot", ".onlysig", "onlypayload.", "a.b.c", "!!!.@@@"} {
		if _, err := pa.Authenticate(proofReq(t, http.MethodGet, "/x", bad)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("proof %q: err = %v, want ErrUnauthenticated", bad, err)
		}
	}
}

// With mTLS possession enabled, a request over a verified TLS connection carrying the
// leaf key as its client certificate is admitted with no signed header.
func TestPossessionAcceptsMTLSChannelBinding(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk, WithTLSPossession())

	r := proofReq(t, http.MethodGet, "/v1/instances", "")
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{PublicKey: leaf.Public()}},
		VerifiedChains:   [][]*x509.Certificate{{{PublicKey: leaf.Public()}}},
	}
	if _, err := pa.Authenticate(r); err != nil {
		t.Fatalf("authenticate over mTLS: %v", err)
	}
}

// mTLS with a client certificate carrying a different key than the token names is refused
// (and, with no signed proof either, the request fails closed).
func TestPossessionRefusesMTLSWrongCert(t *testing.T) {
	leaf, other := dgIdentity(t), dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk, WithTLSPossession())

	r := proofReq(t, http.MethodGet, "/v1/instances", "")
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{PublicKey: other.Public()}},
		VerifiedChains:   [][]*x509.Certificate{{{PublicKey: other.Public()}}},
	}
	if _, err := pa.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// An unverified TLS peer certificate (no verified chains: the server did not require and
// validate client certs) does not count as possession.
func TestPossessionIgnoresUnverifiedTLSCert(t *testing.T) {
	leaf := dgIdentity(t)
	clk := clock.NewManual(time.Unix(1_900_000_000, 0).UTC())
	pa := RequirePossession(fixedAuth{prin: principalFor(leaf)}, clk, WithTLSPossession())

	r := proofReq(t, http.MethodGet, "/v1/instances", "")
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{PublicKey: leaf.Public()}},
		// VerifiedChains deliberately empty.
	}
	if _, err := pa.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}
