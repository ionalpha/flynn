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

// expirySkew is how long before a token's stated expiry the provider treats it as
// due for refresh, so a token is never used in the moment it lapses in flight.
const expirySkew = 30 * time.Second

// defaultTokenTTL is the lifetime assumed when a token endpoint returns no
// expires_in. It is short on purpose: a missing lifetime is refreshed often rather
// than trusted to last.
const defaultTokenTTL = 5 * time.Minute

// maxTokenResponse caps how much of a token-endpoint response is read, so a hostile
// or runaway endpoint cannot exhaust memory.
const maxTokenResponse = 1 << 20 // 1 MiB

// maxTokenTTLSeconds caps the lifetime taken from a token response, so an absurd
// expires_in cannot overflow the duration arithmetic (and wrap negative) or pin a
// stale token in the cache. A token claiming to live longer is simply refreshed at
// the cap.
const maxTokenTTLSeconds = int64(24 * 60 * 60)

// oauth2 obtains a short-lived access token from a token endpoint and sends it as a
// bearer token, refreshing it when it nears expiry. The access token is cached in
// memory only: it is never written to a spec or the vault, and it is re-obtained
// each process from the client credentials or the stored refresh token. Apply is
// safe for concurrent use; one refresh happens at a time.
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

// newOAuth2 builds an oauth2 provider, validating that the config and options carry
// what the chosen grant requires.
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

// Apply writes a current access token as a bearer header, obtaining or refreshing it
// first if the cached one is absent or near expiry.
func (p *oauth2) Apply(ctx context.Context, req *http.Request, src secret.Source) error {
	token, err := p.accessToken(ctx, src)
	if err != nil {
		return err
	}
	return setHeader(req, "Authorization", "Bearer "+token)
}

// accessToken returns a valid access token, refreshing under the lock when the
// cached one is missing or due. Holding the lock across the refresh means concurrent
// callers wait for one refresh rather than each starting their own.
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
	// Refresh ahead of the stated expiry by the skew, but never use less than half the
	// token's life: a short-lived token still gets a margin rather than being used to
	// the last instant.
	effective := ttl - expirySkew
	if effective < ttl/2 {
		effective = ttl / 2
	}
	p.token = token
	p.expiry = p.clk.Now().Add(effective)
	return token, nil
}

// tokenResponse is the subset of an OAuth2 token-endpoint response this needs.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// fetch performs the token-endpoint exchange and returns the access token and its
// lifetime. The client credentials and any refresh token are resolved from the vault
// and exposed only to build the request.
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
		// The transport already classified the failure (a 4xx as Terminal, a
		// retry-exhausted 5xx as Transient, a cancellation as Cancelled), and only
		// returns a nil error for a 2xx/3xx. Preserve that classification rather than
		// recasting every failure as one kind.
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
// resolving the client secret from the vault. The client credentials grant always
// authenticates the client; the refresh grant does so only when a secret is
// configured (a public client sends none).
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
