package integrations

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/integrations/request"
)

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
