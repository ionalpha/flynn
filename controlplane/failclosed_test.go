package controlplane

import (
	"crypto/ed25519"
	"math"
	"net/http"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
)

// outsideScope draws a Scope this package does not define.
func outsideScope() *rapid.Generator[Scope] {
	return rapid.Map(rapid.OneOf(
		rapid.IntRange(math.MinInt32, int(ScopeNone)-1),
		rapid.IntRange(int(ScopeAdmin)+1, math.MaxInt32),
	), func(n int) Scope { return Scope(n) })
}

// TestFailClosedScope proves the zero Scope grants nothing and a Scope outside the scale
// grants nothing, on either side of the comparison. A bare ordered comparison would let a
// scope above [ScopeAdmin] satisfy every route, and a requirement below [ScopeNone]
// admit every caller.
func TestFailClosedScope(t *testing.T) {
	defined := []Scope{ScopeNone, ScopeRead, ScopeOperator, ScopeAdmin}

	t.Run("zero is no access", func(t *testing.T) {
		var zero Scope
		if zero != ScopeNone {
			t.Fatalf("the zero Scope is %v, want %v", zero, ScopeNone)
		}
		for _, req := range []Scope{ScopeRead, ScopeOperator, ScopeAdmin} {
			if zero.Allows(req) {
				t.Fatalf("the zero Scope satisfies %v", req)
			}
		}
		if (Principal{}).Scope.Allows(ScopeRead) {
			t.Fatal("an empty Principal can read")
		}
	})
	t.Run("a scope outside the scale satisfies nothing", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			s := outsideScope().Draw(rt, "scope")
			for _, req := range defined {
				if s.Allows(req) {
					rt.Fatalf("scope %d satisfies %v", int(s), req)
				}
			}
			if s.Allows(s) {
				rt.Fatalf("scope %d satisfies itself", int(s))
			}
		})
	})
	t.Run("a requirement outside the scale is satisfied by nothing", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			req := outsideScope().Draw(rt, "required")
			for _, s := range defined {
				if s.Allows(req) {
					rt.Fatalf("%v satisfies a requirement of scope %d", s, int(req))
				}
			}
		})
	})
	t.Run("an unknown scope is named none", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			if got := outsideScope().Draw(rt, "scope").String(); got != "none" {
				rt.Fatalf("an unknown scope is named %q", got)
			}
		})
	})
	t.Run("the server refuses a principal holding an unknown scope", func(t *testing.T) {
		store, log := newStore(t)
		putWidget(t, store, "w1")
		h := NewServer(store, log, NewTokenAuthenticator(map[string]Principal{
			"high":  {ID: "h", Scope: Scope(99)},
			"low":   {ID: "l", Scope: Scope(-1)},
			"empty": {},
		})).Handler()
		for _, tok := range []string{"high", "low", "empty"} {
			if rec := do(t, h, "/v1/Widget", tok); rec.Code != http.StatusForbidden {
				t.Errorf("token %q read the API: status %d, want 403", tok, rec.Code)
			}
		}
	})
}

// TestFailClosedAuthenticators proves every Authenticator refuses a request it cannot
// positively identify: the zero value, an empty or malformed credential, a table entry
// keyed by the empty string, and an issuer key of the wrong size.
func TestFailClosedAuthenticators(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, err := Issue(issuer, holder.Public(), ScopeAdmin, capability.AllowAll(), dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	t.Run("DenyAll refuses a request carrying a valid delegated token", func(t *testing.T) {
		if _, err := (DenyAll{}).Authenticate(bearerRequest(t, tok)); err == nil {
			t.Fatal("DenyAll authenticated a request")
		}
	})
	t.Run("a zero TokenAuthenticator refuses", func(t *testing.T) {
		var zero TokenAuthenticator
		for _, token := range []string{"", "anything"} {
			if _, err := zero.Authenticate(req(token)); err == nil {
				t.Fatalf("a zero TokenAuthenticator accepted %q", token)
			}
		}
	})
	t.Run("a table entry keyed by the empty string is not a credential", func(t *testing.T) {
		a := NewTokenAuthenticator(map[string]Principal{"": {ID: "x", Scope: ScopeAdmin}})
		blank := req("")
		blank.Header.Set("Authorization", "Bearer ")
		for _, r := range []*http.Request{req(""), blank} {
			if _, err := a.Authenticate(r); err == nil {
				t.Fatal("an empty bearer matched the empty-string table entry")
			}
		}
	})
	t.Run("a malformed Authorization header refuses", func(t *testing.T) {
		a := NewTokenAuthenticator(map[string]Principal{"tok": {ID: "x", Scope: ScopeAdmin}})
		for _, h := range []string{"tok", "Token tok", "Basic tok", "Bearer"} {
			r := req("")
			r.Header.Set("Authorization", h)
			if _, err := a.Authenticate(r); err == nil {
				t.Fatalf("header %q authenticated", h)
			}
		}
	})
	t.Run("a delegation authenticator with no usable issuer refuses a valid token", func(t *testing.T) {
		now := clock.NewManual(dgNow)
		for name, key := range map[string]ed25519.PublicKey{
			"nil":     nil,
			"empty":   {},
			"short":   issuer.Public()[:ed25519.PublicKeySize-1],
			"long":    append(append(ed25519.PublicKey{}, issuer.Public()...), 0),
			"zeroKey": make(ed25519.PublicKey, ed25519.PublicKeySize),
		} {
			a := NewDelegationAuthenticator(key, now)
			if _, err := a.Authenticate(bearerRequest(t, tok)); err == nil {
				t.Errorf("%s issuer accepted a token", name)
			}
		}
	})
	t.Run("a delegation authenticator refuses a request with no token", func(t *testing.T) {
		a := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(dgNow))
		if _, err := a.Authenticate(bearerRequest(t, "")); err == nil {
			t.Fatal("a request with no credential authenticated")
		}
	})
	t.Run("possession refuses a principal whose id is not a key", func(t *testing.T) {
		inner := NewTokenAuthenticator(map[string]Principal{"tok": {ID: "ada", Scope: ScopeAdmin}})
		a := RequirePossession(inner, clock.NewManual(dgNow))
		if _, err := a.Authenticate(req("tok")); err == nil {
			t.Fatal("a principal that is not key-identified skipped the possession check")
		}
	})
}
