package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/secret"
)

// tokenSource yields the credential each API request carries. Two exist: a GitHub
// App, which signs an assertion and exchanges it for a short-lived installation
// token, and a token supplied directly. Everything above this line is identical for
// both, so the auth choice never reaches the tools or the REST calls.
type tokenSource interface {
	// installationToken returns a credential valid at the moment of the call. An
	// implementation that mints one is responsible for its own caching and refresh.
	installationToken(ctx context.Context) (secret.Text, error)
}

// staticSource carries a credential supplied by the caller: a workflow's ambient
// GITHUB_TOKEN, or a personal access token. It expires when whoever issued it says
// so, which is outside this package's knowledge, so there is nothing to refresh.
type staticSource struct{ token secret.Text }

// installationToken returns the caller's token unchanged.
func (s staticSource) installationToken(context.Context) (secret.Text, error) {
	return s.token, nil
}

var _ tokenSource = staticSource{}

var _ tokenSource = (*authenticator)(nil)

// ErrNoCredential is returned when a Config carries neither a GitHub App nor a
// token, so nothing could authenticate a request.
var ErrNoCredential = errors.New("github: Config needs either App or Token")

// ErrAmbiguousCredential is returned when a Config carries both a GitHub App and a
// token. Choosing one silently would decide, on the caller's behalf, which identity
// a review is published under, so it is refused instead.
var ErrAmbiguousCredential = errors.New("github: Config carries both App and Token; set exactly one")

// zero reports whether an App carries no credential at all, which is how a Config
// says it authenticates some other way.
func (a App) zero() bool {
	return a.Issuer == "" && a.InstallationID == 0 && a.PrivateKey == nil
}

// newTokenSource picks the credential path from a Config, refusing a Config that
// names none or both. A partly-filled App is an error rather than a fallback: it
// means someone believes they configured an App, and failing at construction is
// kinder than failing at the first API call.
func newTokenSource(cfg Config) (tokenSource, error) {
	hasToken := !cfg.Token.Empty()
	hasApp := !cfg.App.zero()

	switch {
	case hasToken && hasApp:
		return nil, ErrAmbiguousCredential
	case !hasToken && !hasApp:
		return nil, ErrNoCredential
	case hasToken:
		return staticSource{token: cfg.Token}, nil
	}

	if cfg.App.Issuer == "" {
		return nil, errors.New("github: Config.App.Issuer is required")
	}
	if cfg.App.InstallationID == 0 {
		return nil, errors.New("github: Config.App.InstallationID is required")
	}
	if cfg.App.PrivateKey == nil {
		return nil, errors.New("github: Config.App.PrivateKey is required")
	}
	return &authenticator{
		app:     cfg.App,
		clock:   cfg.Clock,
		http:    cfg.HTTPClient,
		apiBase: cfg.APIBase,
	}, nil
}

// ParsePrivateKey reads the RSA private key a GitHub App downloads at registration.
//
// GitHub issues the key as PKCS#1 ("BEGIN RSA PRIVATE KEY"), but tooling that
// round-trips it through a secret store commonly re-encodes it as PKCS#8 ("BEGIN
// PRIVATE KEY"). Both arrive in the wild, and a caller holding PEM bytes from an
// environment variable cannot tell which it has, so both are accepted.
//
// The key is the App's sole credential and grants everything its installations do.
// It belongs in the vault, and the bytes handed here should come from there.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: private key is not PEM-encoded")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Both decodings failed. Report the PKCS#8 error rather than the PKCS#1 one:
		// a key that is neither is far more often a wrong or truncated file than a
		// malformed PKCS#1 body, and the PKCS#8 parser says which.
		return nil, fmt.Errorf("github: private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is %T, but a GitHub App key is RSA", parsed)
	}
	return key, nil
}
