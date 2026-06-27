package controlplane

import (
	"net/http"
	"testing"

	"pgregory.net/rapid"
)

func req(token string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/v1/Widget", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestTokenAuthenticator(t *testing.T) {
	auth := NewTokenAuthenticator(map[string]Principal{
		"goodtok": {ID: "ada", Scope: ScopeOperator},
	})

	p, err := auth.Authenticate(req("goodtok"))
	if err != nil {
		t.Fatalf("good token: %v", err)
	}
	if p.ID != "ada" || p.Scope != ScopeOperator {
		t.Fatalf("principal = %+v, want {ada operator}", p)
	}

	if _, err := auth.Authenticate(req("wrongtok")); err == nil {
		t.Error("wrong token authenticated, want error")
	}
	if _, err := auth.Authenticate(req("")); err == nil {
		t.Error("missing token authenticated, want error")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc", // scheme is case-insensitive
		"Basic abc":   "",
		"":            "",
		"Bearer":      "",
		"Bearer    x": "   x", // only the first space is the separator
	}
	for header, want := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

// TestScopeAllowsProperty is the rigor property: scopes are totally ordered, so a
// scope satisfies a requirement exactly when it is at least as high.
func TestScopeAllowsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := Scope(rapid.IntRange(0, 3).Draw(rt, "a"))
		b := Scope(rapid.IntRange(0, 3).Draw(rt, "b"))
		if a.Allows(b) != (a >= b) {
			rt.Fatalf("%s.Allows(%s) = %v, want %v", a, b, a.Allows(b), a >= b)
		}
	})
}

func TestDenyAllRefusesEverything(t *testing.T) {
	var a Authenticator = DenyAll{}
	// A valid-looking bearer, an empty one: both refused.
	for _, tok := range []string{"", "anything", "Bearer-shaped"} {
		if _, err := a.Authenticate(req(tok)); err == nil {
			t.Errorf("DenyAll authenticated token %q; must refuse all", tok)
		}
	}
}

func TestGeneratedOperatorMintsAcceptedToken(t *testing.T) {
	mintCalls := 0
	mint := func() (string, error) { mintCalls++; return "minted-secret-token", nil }
	auth, tok, err := GeneratedOperator("operator", ScopeRead, mint)
	if err != nil {
		t.Fatalf("GeneratedOperator: %v", err)
	}
	if mintCalls != 1 {
		t.Errorf("mint called %d times, want 1", mintCalls)
	}
	if tok != "minted-secret-token" {
		t.Errorf("returned token %q, want the minted one", tok)
	}
	// The minted token authenticates as a read-scoped operator.
	p, err := auth.Authenticate(req(tok))
	if err != nil {
		t.Fatalf("minted token should authenticate: %v", err)
	}
	if p.ID != "operator" || p.Scope != ScopeRead {
		t.Errorf("principal = %+v, want operator/read", p)
	}
	// A different token does not.
	if _, err := auth.Authenticate(req("wrong")); err == nil {
		t.Error("a non-minted token must be refused")
	}
}

func TestGeneratedOperatorRejectsEmptyMint(t *testing.T) {
	if _, _, err := GeneratedOperator("operator", ScopeRead, func() (string, error) { return "", nil }); err == nil {
		t.Error("an empty generated token must be refused, not accepted as no-auth")
	}
}
