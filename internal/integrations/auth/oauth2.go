package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// expirySkew is how long before stated expiry a token is refreshed, so it never
// lapses in flight.
const expirySkew = 30 * time.Second

// defaultTokenTTL is the lifetime assumed when a token endpoint returns no
// expires_in. It is short so a missing lifetime is refreshed often.
const defaultTokenTTL = 5 * time.Minute

// maxTokenResponse caps how much of a token-endpoint response is read, so a hostile
// endpoint cannot exhaust memory.
const maxTokenResponse = 1 << 20 // 1 MiB

// maxTokenTTLSeconds caps the lifetime taken from a token response, so an absurd
// expires_in cannot overflow the duration arithmetic or pin a stale token. A longer
// claim is refreshed at the cap.
const maxTokenTTLSeconds = int64(24 * 60 * 60)

// oauth2 fetches a short-lived access token from a token endpoint, sends it as a
// bearer token and refreshes it near expiry. The token lives in memory only, never in
// a spec or the vault, and is fetched again each process. Apply is safe for
// concurrent use; one refresh runs at a time.
type oauth2 struct {
	tokenURL        string
	clientID        string
	clientSecretRef string
	refreshTokenRef string
	grant           string
	scopes          []string

	exchanger TokenExchanger
	clk       clock.Clock

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// newOAuth2 builds an oauth2 provider, checking the config and options carry what
// the grant requires.
func newOAuth2(c Config, o options) (Provider, error) {
	if c.TokenURL == "" {
		return nil, fault.New(fault.Terminal, "auth_config", "oauth2 needs a token_url")
	}
	grant := c.Grant
	if grant == "" {
		grant = GrantClientCredentials
	}
	switch grant {
	case GrantClientCredentials:
		if c.ClientID == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "oauth2 client_credentials needs a client_id")
		}
	case GrantRefreshToken:
		if c.RefreshTokenRef == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "oauth2 refresh_token grant needs a refresh_token_ref")
		}
	default:
		return nil, fault.New(fault.Terminal, "auth_config", "oauth2 grant must be client_credentials or refresh_token")
	}
	if o.exchanger == nil {
		return nil, fault.New(fault.Terminal, "auth_config", "oauth2 needs a token exchanger")
	}
	clk := o.clk
	if clk == nil {
		clk = clock.System{}
	}
	return &oauth2{
		tokenURL:        c.TokenURL,
		clientID:        c.ClientID,
		clientSecretRef: c.ClientSecretRef,
		refreshTokenRef: c.RefreshTokenRef,
		grant:           grant,
		scopes:          c.Scopes,
		exchanger:       o.exchanger,
		clk:             clk,
	}, nil
}

func (*oauth2) Scheme() Scheme { return SchemeOAuth2 }

// Apply writes a current access token as a bearer header, fetching or refreshing it
// first when absent or near expiry.
func (p *oauth2) Apply(ctx context.Context, req *http.Request, src secret.Source) error {
	token, err := p.accessToken(ctx, src)
	if err != nil {
		return err
	}
	return setHeader(req, "Authorization", "Bearer "+token)
}

// accessToken returns a valid access token, refreshing under the lock when it is
// missing or due, so concurrent callers wait for one refresh.
func (p *oauth2) accessToken(ctx context.Context, src secret.Source) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.clk.Now().Before(p.expiry) {
		return p.token, nil
	}
	token, ttl, err := p.fetch(ctx, src)
	if err != nil {
		return "", err
	}
	// Refresh the skew ahead of expiry but never at less than half the token's life,
	// so a short-lived token still gets a margin.
	effective := ttl - expirySkew
	if effective < ttl/2 {
		effective = ttl / 2
	}
	p.token = token
	p.expiry = p.clk.Now().Add(effective)
	return token, nil
}

// tokenResponse is the subset of an OAuth2 token-endpoint response read here.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// fetch performs the token exchange and returns the access token and its lifetime.
// The client credentials and refresh token come from the vault and are exposed only
// to build the request.
func (p *oauth2) fetch(ctx context.Context, src secret.Source) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", p.grant)
	if len(p.scopes) > 0 {
		form.Set("scope", strings.Join(p.scopes, " "))
	}
	if p.grant == GrantRefreshToken {
		rt, err := resolve(ctx, src, p.refreshTokenRef)
		if err != nil {
			return "", 0, err
		}
		defer rt.Destroy()
		form.Set("refresh_token", rt.Expose())
	}
	body := form.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(body))
	if err != nil {
		return "", 0, fault.Wrap(fault.Terminal, "oauth2_request", err)
	}
	raw := body
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(raw)), nil }
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := p.setClientAuth(ctx, req, src); err != nil {
		return "", 0, err
	}

	resp, derr := p.exchanger.Do(ctx, req)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if derr != nil {
		// The transport already classified the failure (4xx Terminal, a 5xx after
		// retries Transient, cancellation Cancelled) and returns nil only for 2xx and
		// 3xx, so the classification is kept.
		return "", 0, derr
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return "", 0, fault.Wrap(fault.Transient, "oauth2_token_read", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(payload, &tr); err != nil {
		return "", 0, fault.Wrap(fault.Terminal, "oauth2_token_decode", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fault.New(fault.Terminal, "oauth2_token_missing", "oauth2 token response had no access_token")
	}
	ttl := defaultTokenTTL
	if tr.ExpiresIn > 0 {
		secs := tr.ExpiresIn
		if secs > maxTokenTTLSeconds {
			secs = maxTokenTTLSeconds
		}
		ttl = time.Duration(secs) * time.Second
	}
	return tr.AccessToken, ttl, nil
}

// setClientAuth adds HTTP Basic client authentication when a client id is set,
// resolving the secret from the vault. client_credentials always authenticates the
// client; refresh_token does only when a secret is configured, since a public client
// sends none.
func (p *oauth2) setClientAuth(ctx context.Context, req *http.Request, src secret.Source) error {
	if p.clientID == "" {
		return nil
	}
	secretValue := ""
	if p.clientSecretRef != "" {
		cs, err := resolve(ctx, src, p.clientSecretRef)
		if err != nil {
			return err
		}
		defer cs.Destroy()
		secretValue = cs.Expose()
	}
	creds := p.clientID + ":" + secretValue
	return setHeader(req, "Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
}
