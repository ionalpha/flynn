package controlplane

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
)

// bearerRequest builds a GET carrying an "Authorization: Bearer <tok>" header, or no
// header at all when tok is empty, so a test can present (or withhold) a credential.
func bearerRequest(t *testing.T, tok string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/v1/instances", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

// TestDelegationAuthenticatorResolvesPrincipal proves the bridge from a verified token
// to the request model: a valid token yields a Principal carrying the subject, the
// scope, and the exact verified Grant the action gate later checks.
func TestDelegationAuthenticatorResolvesPrincipal(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, err := Issue(issuer, holder.Public(), ScopeOperator, capability.NewGrant("a", "b"), dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	auth := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(dgNow))

	p, err := auth.Authenticate(bearerRequest(t, tok))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.ID != holder.ID() {
		t.Fatalf("principal id = %q, want the leaf subject %q", p.ID, holder.ID())
	}
	if p.Scope != ScopeOperator {
		t.Fatalf("principal scope = %v, want operator", p.Scope)
	}
	if !p.Grant.Allows("a") || !p.Grant.Allows("b") || p.Grant.Allows("c") {
		t.Fatalf("principal grant = %v, want exactly {a,b}", p.Grant.Actions())
	}
}

// TestDelegationAuthenticatorCarriesAttenuatedGrant proves the no-escalation property
// survives the bridge: a delegated, narrowed token resolves to a Principal whose Grant
// is the intersection, not the issuer's original authority.
func TestDelegationAuthenticatorCarriesAttenuatedGrant(t *testing.T) {
	issuer, h1, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	root, err := Issue(issuer, h1.Public(), ScopeOperator, capability.NewGrant("a", "b"), dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	att, err := Attenuate(root, h1, h2.Public(), ScopeRead, []string{"a"}, dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("attenuate: %v", err)
	}
	auth := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(dgNow))

	p, err := auth.Authenticate(bearerRequest(t, att))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.ID != h2.ID() {
		t.Fatalf("principal id = %q, want the delegated leaf %q", p.ID, h2.ID())
	}
	if p.Scope != ScopeRead {
		t.Fatalf("principal scope = %v, want read after narrowing", p.Scope)
	}
	if !p.Grant.Allows("a") || p.Grant.Allows("b") {
		t.Fatalf("principal grant = %v, want exactly {a} after attenuation", p.Grant.Actions())
	}
}

// TestDelegationAuthenticatorRefusesMissingToken: no credential is unauthenticated,
// never a partial principal.
func TestDelegationAuthenticatorRefusesMissingToken(t *testing.T) {
	issuer := dgIdentity(t)
	auth := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(dgNow))
	if _, err := auth.Authenticate(bearerRequest(t, "")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated for a missing token", err)
	}
}

// TestDelegationAuthenticatorRefusesForgedIssuer: a token verifies only against its
// own issuer key, and a refusal is the opaque ErrUnauthenticated, not the underlying
// verify error, so a probing client learns nothing about why it failed.
func TestDelegationAuthenticatorRefusesForgedIssuer(t *testing.T) {
	issuer, attacker, holder := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), dgNow.Add(time.Hour))
	auth := NewDelegationAuthenticator(attacker.Public(), clock.NewManual(dgNow))
	if _, err := auth.Authenticate(bearerRequest(t, tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated for a wrong-issuer token", err)
	}
}

// TestDelegationAuthenticatorRefusesExpired: expiry is checked against the injected
// clock, so a token past its NotAfter is unauthenticated.
func TestDelegationAuthenticatorRefusesExpired(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), dgNow.Add(time.Minute))
	// Advance the clock past expiry; the same token that would verify now does not.
	auth := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(dgNow.Add(time.Hour)))
	if _, err := auth.Authenticate(bearerRequest(t, tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated for an expired token", err)
	}
}

// TestDelegationAuthenticatorNilClockUsesSystem: the constructor never produces a nil
// clock that would panic on the first request.
func TestDelegationAuthenticatorNilClockUsesSystem(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	// Far-future expiry (sourced through the sanctioned clock, never time.Now directly)
	// so the system-clock fallback still considers the token valid.
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), clock.System{}.Now().Add(24*time.Hour))
	auth := NewDelegationAuthenticator(issuer.Public(), nil)
	if _, err := auth.Authenticate(bearerRequest(t, tok)); err != nil {
		t.Fatalf("authenticate with a nil clock: %v", err)
	}
}
