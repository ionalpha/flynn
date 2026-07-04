package integrations

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/integrations/request"
)

// oauth2AuthSpec is the manifest auth block shared by the caching tests: an oauth2
// integration whose client secret comes from the resolved credential.
func oauth2AuthSpec() extension.AuthSpec {
	return extension.AuthSpec{
		Type:          "oauth2",
		CredentialRef: "svc",
		OAuth2: &extension.OAuth2Spec{
			TokenURL: "https://idp.example.com/token",
			ClientID: "client-id",
			Grant:    "client_credentials",
			Scopes:   []string{"api.read"},
		},
	}
}

// oauth2OpBlock is a one-operation integration that issues a single GET.
const oauth2OpBlock = `{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/thing"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`

// clientSecretFromBasic decodes the "Basic base64(client-id:secret)" client
// authentication a token request carries and returns the secret, so a test can prove
// which vault reference a token fetch resolved.
func clientSecretFromBasic(t *testing.T, header string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		t.Fatalf("decode basic auth %q: %v", header, err)
	}
	_, secret, ok := strings.Cut(string(raw), ":")
	if !ok {
		t.Fatalf("basic auth not user:pass: %q", raw)
	}
	return secret
}

// TestOAuth2ProviderCachedAcrossCalls is the M11 performance gate: the binding builds the
// auth provider once and reuses it, so an oauth2 integration fetches a token from the
// endpoint exactly once and reuses it across operations. Before the provider was cached,
// each call built a fresh oauth2 provider with an empty token cache and paid an extra
// token round trip, so the endpoint was hit once per operation (hits == M). With the
// cache it is hit once (hits == 1) until the token expires.
func TestOAuth2ProviderCachedAcrossCalls(t *testing.T) {
	var tokenHits, apiHits int
	doer := &fakeDoer{reply: func(req *http.Request) (int, string) {
		if strings.Contains(req.URL.Host, "idp") {
			tokenHits++
			return 200, `{"access_token":"at-xyz","expires_in":3600}`
		}
		apiHits++
		return 200, `{"ok":true}`
	}}

	store := credStore(
		t,
		credential.Spec{Integration: "svc", Name: "default", AuthType: "oauth2", IsDefault: true},
	)
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"svc/default": "client-secret"}),
		WithCredentials(store),
		WithClock(clock.NewManual(time.Unix(0, 0).UTC())),
	)
	loader, _ := loadExtension(t, h, oauth2AuthSpec(), "https://api.example.com", oauth2OpBlock)
	tool := loader.Tools()[0]

	const ops = 5
	for i := range ops {
		if _, err := tool.Invoke(context.Background(), nil); err != nil {
			t.Fatalf("invoke %d: %v", i, err)
		}
	}
	if apiHits != ops {
		t.Fatalf("expected %d api calls, got %d", ops, apiHits)
	}
	if tokenHits != 1 {
		t.Fatalf("token endpoint hit %d times across %d operations; the provider and its token should be cached, so it must be fetched once", tokenHits, ops)
	}
}

// TestOAuth2ProviderRebuiltOnCredentialRotation proves the cache does not pin a stale
// provider: the key includes the credential's effective vault reference, so rotating the
// credential to a new secret location rebuilds the provider and fetches a fresh token
// authenticated with the new secret, rather than serving the token minted from the old
// one. This is the invariant that keeps the cache safe (a rotation takes effect at once).
func TestOAuth2ProviderRebuiltOnCredentialRotation(t *testing.T) {
	var tokenSecrets []string
	doer := &fakeDoer{reply: func(req *http.Request) (int, string) {
		if strings.Contains(req.URL.Host, "idp") {
			tokenSecrets = append(tokenSecrets, clientSecretFromBasic(t, req.Header.Get("Authorization")))
			return 200, `{"access_token":"at-xyz","expires_in":3600}`
		}
		return 200, `{"ok":true}`
	}}

	store := credStore(
		t,
		credential.Spec{Integration: "svc", Name: "default", AuthType: "oauth2", IsDefault: true},
	)
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"svc/default": "secret-old", "svc/rotated": "secret-new"}),
		WithCredentials(store),
		WithClock(clock.NewManual(time.Unix(0, 0).UTC())),
	)
	loader, _ := loadExtension(t, h, oauth2AuthSpec(), "https://api.example.com", oauth2OpBlock)
	tool := loader.Tools()[0]

	// First two calls share the cached provider: one token fetch with the old secret.
	for i := range 2 {
		if _, err := tool.Invoke(context.Background(), nil); err != nil {
			t.Fatalf("pre-rotation invoke %d: %v", i, err)
		}
	}

	// Rotate the credential to a new vault reference (its secret now lives at svc/rotated).
	if _, err := store.Put(context.Background(), credential.Spec{
		Integration: "svc", Name: "default", AuthType: "oauth2", IsDefault: true, VaultRef: "svc/rotated",
	}); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	if _, err := tool.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("post-rotation invoke: %v", err)
	}

	if len(tokenSecrets) != 2 {
		t.Fatalf("expected 2 token fetches (one before, one after rotation), got %d: %v", len(tokenSecrets), tokenSecrets)
	}
	if tokenSecrets[0] != "secret-old" {
		t.Fatalf("first token fetch used %q, want the pre-rotation secret", tokenSecrets[0])
	}
	if tokenSecrets[1] != "secret-new" {
		t.Fatalf("post-rotation token fetch used %q, want the rotated secret; the provider was not rebuilt", tokenSecrets[1])
	}
}

// TestOAuth2ProviderCachedUnderConcurrency exercises the cache the way fan-out does: many
// operations of one binding invoked at once. The provider cache and the oauth2 provider
// are both mutex-guarded, so the token endpoint is still fetched exactly once and the run
// is free of data races (this test is meaningful under -race).
func TestOAuth2ProviderCachedUnderConcurrency(t *testing.T) {
	var tokenHits, apiHits atomic.Int64
	// A dedicated doer keeping no shared request state, so the harness itself is safe to
	// call from many goroutines (fakeDoer records the last request into unguarded fields).
	doer := concurrentDoerFunc(func(req *http.Request) (int, string) {
		if strings.Contains(req.URL.Host, "idp") {
			tokenHits.Add(1)
			return 200, `{"access_token":"at-xyz","expires_in":3600}`
		}
		apiHits.Add(1)
		return 200, `{"ok":true}`
	})

	store := credStore(
		t,
		credential.Spec{Integration: "svc", Name: "default", AuthType: "oauth2", IsDefault: true},
	)
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"svc/default": "client-secret"}),
		WithCredentials(store),
		WithClock(clock.NewManual(time.Unix(0, 0).UTC())),
	)
	loader, _ := loadExtension(t, h, oauth2AuthSpec(), "https://api.example.com", oauth2OpBlock)
	tool := loader.Tools()[0]

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := tool.Invoke(context.Background(), nil); err != nil {
				t.Errorf("concurrent invoke: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := apiHits.Load(); got != goroutines {
		t.Fatalf("expected %d api calls, got %d", goroutines, got)
	}
	if got := tokenHits.Load(); got != 1 {
		t.Fatalf("token endpoint hit %d times across %d concurrent operations; one shared cached provider must fetch once", got, goroutines)
	}
}

// concurrentDoerFunc is a request.Doer that maps a request to a canned (status, body)
// through fn and keeps no shared state, so it is safe to call concurrently. fn must be
// safe for concurrent use.
type concurrentDoerFunc func(*http.Request) (int, string)

func (f concurrentDoerFunc) Do(req *http.Request) (*http.Response, error) {
	status, body := f(req)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// TestOAuth2IntegrationEndToEnd proves an oauth2 integration obtains a token from the
// token endpoint and signs the API request with it, all through one governed
// transport. The credential supplies the client secret; the static oauth2 parameters
// come from the manifest.
func TestOAuth2IntegrationEndToEnd(t *testing.T) {
	var tokenAuth, apiAuth string
	doer := &fakeDoer{reply: func(req *http.Request) (int, string) {
		if strings.Contains(req.URL.Host, "idp") {
			tokenAuth = req.Header.Get("Authorization")
			return 200, `{"access_token":"at-xyz","expires_in":3600}`
		}
		apiAuth = req.Header.Get("Authorization")
		return 200, `{"ok":true}`
	}}

	store := credStore(
		t,
		credential.Spec{Integration: "svc", Name: "default", AuthType: "oauth2", IsDefault: true},
	)
	h := NewHandler(
		WithTransport(request.New(request.WithDoer(doer))),
		WithSecrets(fakeSecrets{"svc/default": "client-secret"}),
		WithCredentials(store),
		WithClock(clock.NewManual(time.Unix(0, 0).UTC())),
	)

	authSpec := extension.AuthSpec{
		Type:          "oauth2",
		CredentialRef: "svc",
		OAuth2: &extension.OAuth2Spec{
			TokenURL: "https://idp.example.com/token",
			ClientID: "client-id",
			Grant:    "client_credentials",
			Scopes:   []string{"api.read"},
		},
	}
	loader, _ := loadExtension(t, h, authSpec, "https://api.example.com",
		`{"operations":[{"name":"get","flow":{"steps":[{"id":"r","op":"http","http":{"url":"/thing"}},{"op":"return","return":{"value":"{{steps.r.status}}"}}]}}]}`)

	if _, err := loader.Tools()[0].Invoke(context.Background(), nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// The token request authenticated the client with Basic auth.
	if !strings.HasPrefix(tokenAuth, "Basic ") {
		t.Fatalf("token request client auth: %q", tokenAuth)
	}
	// The API request carried the obtained bearer token.
	if apiAuth != "Bearer at-xyz" {
		t.Fatalf("api request auth: %q", apiAuth)
	}
}
