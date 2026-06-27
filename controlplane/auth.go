// Package controlplane is the authenticated API surface over the agent's resource
// store: the one boundary the CLI, the web dashboard, and remote peers read and act
// through. This file is the authentication and authorization model; server.go
// serves the read/watch API over it.
//
// The model is scope-based and identity-first: every request resolves to a
// Principal carrying a Scope, and a handler requires a minimum scope. Local,
// single-operator use authenticates with a bearer token; cross-instance delegated
// authority (public-key, attenuable capability tokens) is a later layer that plugs
// in behind the same Authenticator boundary, so the write and remote paths inherit
// authentication that already exists.
package controlplane

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Scope is the access level behind a request. Scopes are ordered: Admin contains
// Operator contains Read. A handler states the minimum it requires.
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

// Allows reports whether this scope satisfies the required minimum. Because scopes
// are ordered, a higher scope satisfies a lower requirement.
func (s Scope) Allows(required Scope) bool { return s >= required }

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

// Principal is the authenticated identity behind a request, with the scope it is
// granted. It is recorded with every action so the audit trail attributes who did
// what.
type Principal struct {
	ID    string
	Scope Scope
}

// Authenticator resolves a request to a Principal, or an error if it cannot be
// authenticated. Implementations must be safe for concurrent use.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// ErrUnauthenticated means no valid credential was presented.
var ErrUnauthenticated = errors.New("controlplane: unauthenticated")

// DenyAll is an authenticator that refuses every request. It is the fail-closed
// default: a server constructed without an explicit authenticator gets this, so a
// forgotten or misconfigured auth setup locks the door rather than serving openly.
// An unauthenticated API surface is therefore not representable: there is no "no auth"
// mode, only "deny everything" or a real authenticator.
type DenyAll struct{}

// Authenticate always refuses.
func (DenyAll) Authenticate(*http.Request) (Principal, error) { return Principal{}, ErrUnauthenticated }

// GeneratedOperator mints a fresh, cryptographically-random bearer token and returns
// an authenticator that accepts exactly that token as a single operator-scoped
// principal, along with the token so the caller can present it to the operator once.
// It is the auth-on-by-default path: bringing up the API with no operator-supplied
// credential yields a secured-by-default endpoint with zero configuration, so there is
// never a reason to fall back to an open one. The provided mint is the entropy source
// (ids.Token in production); a test can inject a deterministic one.
func GeneratedOperator(id string, scope Scope, mint func() (string, error)) (*TokenAuthenticator, string, error) {
	tok, err := mint()
	if err != nil {
		return nil, "", err
	}
	if tok == "" {
		return nil, "", errors.New("controlplane: refusing an empty generated token")
	}
	return NewTokenAuthenticator(map[string]Principal{tok: {ID: id, Scope: scope}}), tok, nil
}

// TokenAuthenticator authenticates a bearer token against a fixed table. It
// compares in constant time and scans every entry regardless of an early match, so
// neither a token's value nor which tokens exist leaks through timing.
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
