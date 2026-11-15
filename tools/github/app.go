package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/secret"
)

// App is a GitHub App's credentials and the installation it acts as. It holds
// no mutable state, so a Config carrying one is safe to copy.
type App struct {
	// Issuer is the App's client ID (preferred) or numeric App ID: the "iss"
	// claim.
	Issuer string

	// InstallationID is the installation whose token the App mints.
	InstallationID int64

	// PrivateKey signs the assertion. It grants everything the App's
	// installations can do, so keep it in the vault.
	PrivateKey *rsa.PrivateKey
}

// Assertion lifetime. GitHub rejects an assertion whose "exp" is more than ten
// minutes ahead, and backdating "iat" absorbs clock skew between us and GitHub.
const (
	assertionSkew = 60 * time.Second
	assertionTTL  = 9 * time.Minute
)

// tokenRefreshWindow is how long before expiry a cached installation token is
// discarded, so no request outlives its token. Tokens live an hour.
const tokenRefreshWindow = time.Minute

// authenticator mints and caches installation tokens for one App. It is held
// by pointer because it owns mutable state.
type authenticator struct {
	app     App
	clock   clock.Clock
	http    *http.Client
	apiBase string

	mu      sync.Mutex
	token   secret.Text
	expires time.Time
}

// token is safe for concurrent use.
func (a *authenticator) installationToken(ctx context.Context) (secret.Text, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.clock.Now()
	if !a.token.Empty() && now.Before(a.expires.Add(-tokenRefreshWindow)) {
		return a.token, nil
	}

	assertion, err := a.assertion(now)
	if err != nil {
		return secret.Text{}, err
	}
	tok, exp, err := a.exchange(ctx, assertion)
	if err != nil {
		return secret.Text{}, err
	}

	// Wipe the token being replaced so a superseded value does not linger in memory.
	a.token.Destroy()
	a.token, a.expires = tok, exp
	return a.token, nil
}

// assertion signs the RS256 JWT GitHub exchanges for an installation token.
func (a *authenticator) assertion(now time.Time) (secret.Text, error) {
	if a.app.Issuer == "" {
		return secret.Text{}, errors.New("github: App.Issuer is required")
	}
	header, err := jsonSegment(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return secret.Text{}, err
	}
	payload, err := jsonSegment(map[string]any{
		"iat": now.Add(-assertionSkew).Unix(),
		"exp": now.Add(assertionTTL).Unix(),
		"iss": a.app.Issuer,
	})
	if err != nil {
		return secret.Text{}, err
	}

	signing := header + "." + payload
	digest := sha256.Sum256([]byte(signing))
	// SignPKCS1v15 ignores its random parameter. That keeps this off crypto/rand,
	// which is denied outside package ids so replay stays deterministic.
	sig, err := rsa.SignPKCS1v15(nil, a.app.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return secret.Text{}, fmt.Errorf("github: signing app assertion: %w", err)
	}
	return secret.New(signing + "." + encodeSegment(sig)), nil
}

func (a *authenticator) exchange(ctx context.Context, assertion secret.Text) (secret.Text, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, a.app.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return secret.Text{}, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+assertion.Expose())
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := a.http.Do(req)
	if err != nil {
		return secret.Text{}, time.Time{}, fmt.Errorf("github: minting installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return secret.Text{}, time.Time{}, statusError(resp, "mint installation token")
	}

	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return secret.Text{}, time.Time{}, fmt.Errorf("github: decoding installation token: %w", err)
	}
	if body.Token == "" {
		return secret.Text{}, time.Time{}, errors.New("github: installation token response carried no token")
	}
	return secret.New(body.Token), body.ExpiresAt, nil
}

func jsonSegment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("github: encoding app assertion: %w", err)
	}
	return encodeSegment(b), nil
}

func encodeSegment(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
