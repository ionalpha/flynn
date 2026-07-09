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

// App is a GitHub App's credentials and the installation it acts as. It is a
// value type holding no mutable state, so a Config carrying one is safe to copy.
//
// The zero value is unusable. Build one with the App's client ID (or numeric App
// ID), the installation ID, and the RSA private key GitHub issued at registration.
type App struct {
	// Issuer is the App's client ID (preferred) or its numeric App ID as a string.
	// It becomes the "iss" claim of the signed assertion.
	Issuer string

	// InstallationID identifies the installation whose token the App mints. An App
	// with no installation has no repository access.
	InstallationID int64

	// PrivateKey signs the assertion. It is the App's sole credential and grants
	// everything its installations do; store it in the vault, not on disk.
	PrivateKey *rsa.PrivateKey
}

// Assertion lifetime. GitHub rejects an assertion whose "exp" is more than ten
// minutes ahead, and backdating "iat" absorbs clock skew between us and GitHub.
const (
	assertionSkew = 60 * time.Second
	assertionTTL  = 9 * time.Minute
)

// tokenRefreshWindow is how long before expiry a cached installation token is
// discarded. An installation token lives an hour; refreshing a minute early means
// a token is never handed to a request that outlives it.
const tokenRefreshWindow = time.Minute

// authenticator mints and caches installation tokens for one App. It owns the only
// mutable state in this package, so it is always held by pointer.
type authenticator struct {
	app     App
	clock   clock.Clock
	http    *http.Client
	apiBase string

	mu      sync.Mutex
	token   secret.Text
	expires time.Time
}

// token returns a valid installation token, minting a new one when the cache is
// empty or close to expiry. It is safe for concurrent use.
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

// assertion builds and signs the RS256 JSON Web Token GitHub accepts in exchange
// for an installation token.
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
	// The random parameter of SignPKCS1v15 is legacy and unused, so signing needs no
	// entropy source. That matters here: direct crypto/rand is denied outside package
	// ids, because unrouted randomness defeats deterministic replay.
	sig, err := rsa.SignPKCS1v15(nil, a.app.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return secret.Text{}, fmt.Errorf("github: signing app assertion: %w", err)
	}
	return secret.New(signing + "." + encodeSegment(sig)), nil
}

// exchange trades a signed assertion for an installation token and its expiry.
func (a *authenticator) exchange(ctx context.Context, assertion secret.Text) (secret.Text, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, a.app.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
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

// jsonSegment marshals v and encodes it as a base64url JWT segment.
func jsonSegment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("github: encoding app assertion: %w", err)
	}
	return encodeSegment(b), nil
}

// encodeSegment applies the unpadded base64url encoding JWT requires.
func encodeSegment(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
