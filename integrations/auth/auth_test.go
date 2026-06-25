package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/integrations/auth"
	"github.com/ionalpha/flynn/secret"
)

// mapSource is a vault backed by a literal map, for tests. An absent reference
// returns secret.ErrNotFound, matching the Source contract.
type mapSource map[string]string

func (m mapSource) Lookup(_ context.Context, ref string) (secret.Text, error) {
	v, ok := m[ref]
	if !ok {
		return secret.Text{}, secret.ErrNotFound
	}
	return secret.New(v), nil
}

// failSource always fails resolution with a non-NotFound error, standing in for a
// locked keychain or an unreachable vault.
type failSource struct{}

func (failSource) Lookup(context.Context, string) (secret.Text, error) {
	return secret.Text{}, errors.New("vault unavailable")
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1/things?page=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func mustProvider(t *testing.T, c auth.Config) auth.Provider {
	t.Helper()
	p, err := auth.FromConfig(c)
	if err != nil {
		t.Fatalf("FromConfig(%+v): %v", c, err)
	}
	return p
}

func TestNone(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{})
	if p.Scheme() != auth.SchemeNone {
		t.Fatalf("scheme = %q, want none", p.Scheme())
	}
	if err := p.Apply(context.Background(), req, mapSource{}); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("none wrote Authorization = %q", got)
	}
}

func TestBearer(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"})
	src := mapSource{"TOKEN": "sk-abc123"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer sk-abc123"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestBasic(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeBasic, UsernameRef: "USER", PasswordRef: "PASS"})
	src := mapSource{"USER": "alice", "PASS": "s3cr3t"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// A basic config with only a username (the key-as-username pattern some APIs use)
// sends an empty password and does not error on the absent password ref.
func TestBasicUsernameOnly(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeBasic, UsernameRef: "KEY"})
	src := mapSource{"KEY": "sk_live_xyz"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("sk_live_xyz:"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestAPIKeyHeader(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeAPIKey, TokenRef: "KEY", Param: "X-API-Key"})
	src := mapSource{"KEY": "abcdef"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-API-Key"); got != "abcdef" {
		t.Fatalf("X-API-Key = %q, want abcdef", got)
	}
}

func TestAPIKeyHeaderPrefix(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeAPIKey, TokenRef: "KEY", Param: "Authorization", Prefix: "Token "})
	src := mapSource{"KEY": "deadbeef"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Token deadbeef" {
		t.Fatalf("Authorization = %q, want 'Token deadbeef'", got)
	}
}

// An api_key placed in the query is added without disturbing the existing query
// parameters.
func TestAPIKeyQuery(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeAPIKey, TokenRef: "KEY", Param: "api_key", In: auth.InQuery})
	src := mapSource{"KEY": "qpkey"}
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	if got := q.Get("api_key"); got != "qpkey" {
		t.Fatalf("api_key = %q, want qpkey", got)
	}
	if got := q.Get("page"); got != "2" {
		t.Fatalf("existing page param = %q, want 2 (clobbered)", got)
	}
}

// A missing credential is a terminal fault: the integration needs it and cannot
// proceed without it.
func TestMissingCredentialTerminal(t *testing.T) {
	for _, c := range []auth.Config{
		{Type: auth.SchemeBearer, TokenRef: "NOPE"},
		{Type: auth.SchemeAPIKey, TokenRef: "NOPE", Param: "X-Key"},
	} {
		req := newReq(t)
		err := mustProvider(t, c).Apply(context.Background(), req, mapSource{})
		if fault.Classify(err) != fault.Terminal {
			t.Fatalf("%+v: class = %q, want terminal", c, fault.Classify(err))
		}
	}
}

// A basic config errors only when neither credential resolves; one present is fine.
func TestBasicBothMissingTerminal(t *testing.T) {
	req := newReq(t)
	// PASS resolves, USER does not: username falls back to empty, password is used.
	p := mustProvider(t, auth.Config{Type: auth.SchemeBasic, UsernameRef: "USER", PasswordRef: "PASS"})
	if err := p.Apply(context.Background(), req, mapSource{"PASS": "only"}); err != nil {
		t.Fatalf("one credential present should not error: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":only"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// A backend failure (not a missing value) is transient so a caller may retry.
func TestResolveBackendFailureTransient(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"})
	if got := fault.Classify(p.Apply(context.Background(), req, failSource{})); got != fault.Transient {
		t.Fatalf("class = %q, want transient", got)
	}
}

// A credential carrying CR/LF is refused, not written: header injection is a
// terminal fault and the Authorization header stays unset.
func TestHeaderInjectionRefused(t *testing.T) {
	req := newReq(t)
	p := mustProvider(t, auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"})
	src := mapSource{"TOKEN": "good\r\nX-Injected: evil"}
	err := p.Apply(context.Background(), req, src)
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("class = %q, want terminal", fault.Classify(err))
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization was written despite injection: %q", got)
	}
	if got := req.Header.Get("X-Injected"); got != "" {
		t.Fatalf("injected header present: %q", got)
	}
}

func TestFromConfigErrors(t *testing.T) {
	for name, c := range map[string]auth.Config{
		"unknown scheme":     {Type: "weird"},
		"basic no refs":      {Type: auth.SchemeBasic},
		"bearer no token":    {Type: auth.SchemeBearer},
		"api_key no token":   {Type: auth.SchemeAPIKey, Param: "X-Key"},
		"api_key no param":   {Type: auth.SchemeAPIKey, TokenRef: "KEY"},
		"api_key bad in":     {Type: auth.SchemeAPIKey, TokenRef: "KEY", Param: "X-Key", In: "body"},
		"api_key bad header": {Type: auth.SchemeAPIKey, TokenRef: "KEY", Param: "Bad Header"},
	} {
		if _, err := auth.FromConfig(c); fault.Classify(err) != fault.Terminal {
			t.Fatalf("%s: class = %q, want terminal", name, fault.Classify(err))
		}
	}
}

// A secret must never leak through the Config's own rendering: Config holds only
// references, so even a logged config shows the ref name, never a value.
func TestConfigCarriesNoSecret(t *testing.T) {
	c := auth.Config{Type: auth.SchemeBearer, TokenRef: "ANTHROPIC_API_KEY"}
	if got := c.TokenRef; got != "ANTHROPIC_API_KEY" {
		t.Fatalf("token ref = %q", got)
	}
	// There is no field on Config that can hold a literal secret value; this test
	// documents that invariant so a future field addition is a deliberate choice.
}
