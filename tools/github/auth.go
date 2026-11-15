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

// tokenSource yields each request's credential: an App installation token or a
// token supplied directly. The tools and REST calls never see which.
type tokenSource interface {
	// installationToken returns a credential valid now. A minting
	// implementation caches and refreshes it.
	installationToken(ctx context.Context) (secret.Text, error)
}

// staticSource is a caller-supplied token (GITHUB_TOKEN or a personal access
// token). Its expiry is unknown here, so it is never refreshed.
type staticSource struct{ token secret.Text }

func (s staticSource) installationToken(context.Context) (secret.Text, error) {
	return s.token, nil
}

var _ tokenSource = staticSource{}

var _ tokenSource = (*authenticator)(nil)

// ErrNoCredential is returned when a Config has neither an App nor a token.
var ErrNoCredential = errors.New("github: Config needs either App or Token")

// ErrAmbiguousCredential is returned when a Config has both an App and a token,
// because picking one would choose the identity a review is published under.
var ErrAmbiguousCredential = errors.New("github: Config carries both App and Token; set exactly one")

// zero reports an App with no fields set, meaning the Config uses a token.
func (a App) zero() bool {
	return a.Issuer == "" && a.InstallationID == 0 && a.PrivateKey == nil
}

// newTokenSource refuses a partly filled App instead of falling back to the
// token, so a broken App config fails at construction and not at the first
// API call.
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

// ParsePrivateKey reads a GitHub App's PEM key as PKCS#1 ("BEGIN RSA PRIVATE
// KEY", as GitHub issues it) or PKCS#8 ("BEGIN PRIVATE KEY", as secret stores
// often re-encode it), since a caller cannot tell which it holds.
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
		// Report the PKCS#8 error: a key that is neither is usually a wrong or
		// truncated file, and that parser says which.
		return nil, fmt.Errorf("github: private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is %T, but a GitHub App key is RSA", parsed)
	}
	return key, nil
}
