package controlplane

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/ionalpha/flynn/capability"
)

// Scope is the access level behind a request, ordered Read < Operator < Admin. A
// handler states the minimum it requires.
//
//flynn:policy
type Scope int

const (
	// ScopeNone is no access; the zero value, so an unset Principal can do nothing.
	ScopeNone Scope = iota
	// ScopeRead permits get, list, and watch.
	ScopeRead
	// ScopeOperator adds lifecycle and dispatch actions (pause, resume, halt, run).
	ScopeOperator
	// ScopeAdmin adds credentials, configuration, and upgrade.
	ScopeAdmin
)

// Allows reports whether s meets the required minimum. A value outside the defined
// scale answers false on either side, so a corrupt or newer-build scope above
// [ScopeAdmin] cannot pass every route and a requirement below [ScopeNone] cannot
// admit everyone.
func (s Scope) Allows(required Scope) bool {
	return s.defined() && required.defined() && s >= required
}

// defined reports whether s is one of the scopes declared above.
func (s Scope) defined() bool { return s >= ScopeNone && s <= ScopeAdmin }

// String renders the scope for logs and audit.
func (s Scope) String() string {
	switch s {
	case ScopeRead:
		return "read"
	case ScopeOperator:
		return "operator"
	case ScopeAdmin:
		return "admin"
	default:
		return "none"
	}
}

// Principal is the authenticated identity behind a request, recorded with every
// action for audit. Scope is the coarse access level a handler requires. Grant is the
// action authority: capability.AllowAll for a local operator token, or the attenuated
// Grant a delegated token proved. The action gate intersects Grant with the target's
// local grant. The zero Grant denies everything, so an unauthenticated Principal has
// no authority.
type Principal struct {
	ID    string
	Scope Scope
	Grant capability.Grant
}

// Authenticator resolves a request to a Principal, or an error if it cannot be
// authenticated. Implementations must be safe for concurrent use.
//
//flynn:policy TestFailClosedAuthenticators
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// ErrUnauthenticated means no valid credential was presented.
var ErrUnauthenticated = errors.New("controlplane: unauthenticated")

// DenyAll refuses every request. A server built without an authenticator gets it, so
// a forgotten auth setup locks the API instead of serving it openly; there is no
// unauthenticated mode.
type DenyAll struct{}

// Authenticate always refuses.
func (DenyAll) Authenticate(*http.Request) (Principal, error) { return Principal{}, ErrUnauthenticated }

// GeneratedOperator mints a random bearer token and returns an authenticator that
// accepts only it, as one principal with the given scope, plus the token to show the
// operator once. The API can then start secured with no configuration. mint is the
// entropy source (ids.Token in production).
func GeneratedOperator(id string, scope Scope, mint func() (string, error)) (*TokenAuthenticator, string, error) {
	tok, err := mint()
	if err != nil {
		return nil, "", err
	}
	if tok == "" {
		return nil, "", errors.New("controlplane: refusing an empty generated token")
	}
	// A local operator token is bounded only by its scope, so it carries AllowAll and the
	// action gate reduces to the instance's local grant.
	return NewTokenAuthenticator(map[string]Principal{tok: {ID: id, Scope: scope, Grant: capability.AllowAll()}}), tok, nil
}

// TokenAuthenticator authenticates a bearer token against a fixed table. It
// compares in constant time and scans every entry regardless of an early match, so
// neither a token's value nor which tokens exist leaks through timing.
//
//flynn:policy TestFailClosedAuthenticators
type TokenAuthenticator struct {
	tokens map[string]Principal
}

// NewTokenAuthenticator builds an authenticator over a token -> Principal table.
func NewTokenAuthenticator(tokens map[string]Principal) *TokenAuthenticator {
	m := make(map[string]Principal, len(tokens))
	for t, p := range tokens {
		m[t] = p
	}
	return &TokenAuthenticator{tokens: m}
}

// Authenticate resolves the request's bearer token to its Principal.
func (a *TokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	tok := bearerToken(r)
	if tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	var (
		found Principal
		match bool
	)
	for t, p := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			found, match = p, true
		}
	}
	if !match {
		return Principal{}, ErrUnauthenticated
	}
	return found, nil
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// or "" when absent.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}
