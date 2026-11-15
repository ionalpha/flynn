// Package auth applies an integration's credentials to an outbound HTTP request. A
// Config (plain data in an integration spec) selects a Provider, and every surface
// calling a remote API (the API tool, the scraper) calls Apply before handing the
// request to the shared transport, so all authenticate the same way.
//
// A Config names the scheme and the vault references to resolve, never a
// credential. Apply looks each reference up through a secret.Source at call time
// and exposes the secret.Text once, where it is written onto the request, so a
// credential never reaches a logged config, an event or the spec.
//
// Header names and values are validated against the HTTP grammar before they are
// set, so a hostile credential, prefix or parameter name cannot smuggle CR/LF into
// a second header. Query keys go through url.Values, which percent-encodes them.
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// Scheme names an authentication mechanism, the discriminant of a Config.
type Scheme string

const (
	// SchemeNone applies no credentials. It is the zero value and the explicit
	// public-API choice.
	SchemeNone Scheme = "none"
	// SchemeBasic sends RFC 7617 HTTP Basic credentials in the Authorization header.
	SchemeBasic Scheme = "basic"
	// SchemeBearer sends a token as "Authorization: Bearer <token>".
	SchemeBearer Scheme = "bearer"
	// SchemeAPIKey sends a key in a named header or query parameter, with an
	// optional value prefix.
	SchemeAPIKey Scheme = "api_key"
	// SchemeOAuth2 fetches a short-lived access token from a token endpoint, sends
	// it as a bearer token and refreshes it on expiry.
	SchemeOAuth2 Scheme = "oauth2"
)

// Placement is where SchemeAPIKey puts the key.
type Placement string

const (
	// InHeader places the key in a request header, the default.
	InHeader Placement = "header"
	// InQuery places the key in a URL query parameter.
	InQuery Placement = "query"
)

// Config is the data form of an integration's auth: the scheme, vault references and
// placement. It holds no secret values, so it is safe to serialize, log and store in
// a spec. The zero Config is SchemeNone.
type Config struct {
	Type Scheme `json:"type,omitempty"`

	// TokenRef is the vault reference for SchemeBearer and SchemeAPIKey.
	TokenRef string `json:"token_ref,omitempty"`

	// UsernameRef and PasswordRef are the vault references for SchemeBasic. Either
	// may be empty, sending an empty value, but a config resolving neither is an
	// error.
	UsernameRef string `json:"username_ref,omitempty"`
	PasswordRef string `json:"password_ref,omitempty"`

	// In selects header or query placement for SchemeAPIKey. Empty means InHeader.
	In Placement `json:"in,omitempty"`
	// Param is the header or query-parameter name for SchemeAPIKey ("X-API-Key",
	// "api_key"). Required for SchemeAPIKey.
	Param string `json:"param,omitempty"`
	// Prefix is a literal prepended to the SchemeAPIKey value ("Token ", "Bearer ").
	// It is part of the wire value and is no secret.
	Prefix string `json:"prefix,omitempty"`

	// OAuth2 fields (SchemeOAuth2). TokenURL is the token endpoint; ClientID is the
	// semi-public client identifier, carried inline; ClientSecretRef and
	// RefreshTokenRef are vault references. Grant is "client_credentials" (default)
	// or "refresh_token"; Scopes are requested at the endpoint. The access token is
	// cached and refreshed, never stored in a spec.
	TokenURL        string   `json:"token_url,omitempty"`
	ClientID        string   `json:"client_id,omitempty"`
	ClientSecretRef string   `json:"client_secret_ref,omitempty"`
	RefreshTokenRef string   `json:"refresh_token_ref,omitempty"`
	Grant           string   `json:"grant,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
}

// OAuth2 grant types.
const (
	// GrantClientCredentials authenticates with the client's own credentials, for
	// server-to-server access.
	GrantClientCredentials = "client_credentials"
	// GrantRefreshToken exchanges a stored refresh token for an access token.
	GrantRefreshToken = "refresh_token"
)

// TokenExchanger performs the token-endpoint request for SchemeOAuth2. The shared
// request transport satisfies it, so a token request takes the same governed path
// (anti-SSRF dialer, bounded retries) as every other request.
type TokenExchanger interface {
	Do(ctx context.Context, req *http.Request) (*http.Response, error)
}

// options are a stateful provider's dependencies beyond its Config. SchemeOAuth2
// requires a TokenExchanger and tracks token expiry on the clock; static schemes
// ignore them.
type options struct {
	exchanger TokenExchanger
	clk       clock.Clock
}

// Option configures FromConfig.
type Option func(*options)

// WithTokenExchanger supplies the transport SchemeOAuth2 calls the token endpoint
// through. An oauth2 provider requires it.
func WithTokenExchanger(e TokenExchanger) Option {
	return func(o *options) { o.exchanger = e }
}

// WithClock supplies the clock SchemeOAuth2 measures token expiry on (default
// clock.System).
func WithClock(c clock.Clock) Option {
	return func(o *options) {
		if c != nil {
			o.clk = c
		}
	}
}

// Provider applies one auth scheme to a request. A surface calls Apply on every
// outbound request before dispatching it through the transport.
type Provider interface {
	// Apply resolves the credentials from src and writes them onto req. A missing
	// credential (secret.ErrNotFound) is a terminal fault, since the integration
	// needs it. Apply never writes a header it could not validate.
	Apply(ctx context.Context, req *http.Request, src secret.Source) error
	// Scheme reports the mechanism, for audit and introspection.
	Scheme() Scheme
}

// FromConfig builds the Provider c selects, checking that c carries the references
// its scheme requires. An unknown scheme or a missing field is a terminal
// configuration fault. The zero Config and SchemeNone yield the no-op provider.
// SchemeOAuth2 requires WithTokenExchanger; other schemes ignore the options.
func FromConfig(c Config, opts ...Option) (Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	switch c.Type {
	case "", SchemeNone:
		return none{}, nil
	case SchemeBasic:
		if c.UsernameRef == "" && c.PasswordRef == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "basic auth needs a username_ref or password_ref")
		}
		return basic{usernameRef: c.UsernameRef, passwordRef: c.PasswordRef}, nil
	case SchemeBearer:
		if c.TokenRef == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "bearer auth needs a token_ref")
		}
		return bearer{tokenRef: c.TokenRef}, nil
	case SchemeAPIKey:
		if c.TokenRef == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "api_key auth needs a token_ref")
		}
		if c.Param == "" {
			return nil, fault.New(fault.Terminal, "auth_config", "api_key auth needs a param name")
		}
		in := c.In
		if in == "" {
			in = InHeader
		}
		if in != InHeader && in != InQuery {
			return nil, fault.New(fault.Terminal, "auth_config", "api_key 'in' must be header or query")
		}
		if in == InHeader && !validHeaderName(c.Param) {
			return nil, fault.New(fault.Terminal, "auth_config", "api_key param is not a valid header name")
		}
		return apiKey{tokenRef: c.TokenRef, in: in, param: c.Param, prefix: c.Prefix}, nil
	case SchemeOAuth2:
		return newOAuth2(c, o)
	default:
		return nil, fault.New(fault.Terminal, "auth_config", "unknown auth scheme: "+string(c.Type))
	}
}

type none struct{}

func (none) Apply(context.Context, *http.Request, secret.Source) error { return nil }
func (none) Scheme() Scheme                                            { return SchemeNone }

// basic sends RFC 7617 Basic credentials. The base64 of "user:pass" is always
// header-safe, so the value is not validated.
type basic struct {
	usernameRef string
	passwordRef string
}

func (b basic) Apply(ctx context.Context, req *http.Request, src secret.Source) error {
	user, err := resolveOptional(ctx, src, b.usernameRef)
	if err != nil {
		return err
	}
	pass, err := resolveOptional(ctx, src, b.passwordRef)
	if err != nil {
		return err
	}
	defer user.Destroy()
	defer pass.Destroy()
	raw := user.Expose() + ":" + pass.Expose()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	return nil
}

func (basic) Scheme() Scheme { return SchemeBasic }

type bearer struct{ tokenRef string }

func (b bearer) Apply(ctx context.Context, req *http.Request, src secret.Source) error {
	tok, err := resolve(ctx, src, b.tokenRef)
	if err != nil {
		return err
	}
	defer tok.Destroy()
	return setHeader(req, "Authorization", "Bearer "+tok.Expose())
}

func (bearer) Scheme() Scheme { return SchemeBearer }

type apiKey struct {
	tokenRef string
	in       Placement
	param    string
	prefix   string
}

func (a apiKey) Apply(ctx context.Context, req *http.Request, src secret.Source) error {
	tok, err := resolve(ctx, src, a.tokenRef)
	if err != nil {
		return err
	}
	defer tok.Destroy()
	value := a.prefix + tok.Expose()
	if a.in == InQuery {
		// url.Values.Encode percent-encodes name and value, so hostile bytes cannot
		// alter the URL structure.
		q := req.URL.Query()
		q.Set(a.param, value)
		req.URL.RawQuery = q.Encode()
		return nil
	}
	return setHeader(req, a.param, value)
}

func (a apiKey) Scheme() Scheme { return SchemeAPIKey }

// resolve looks a required reference up in the vault. A missing value is terminal,
// since the integration declared it. A backend failure (a locked keychain, an
// unreachable vault) is transient and retryable.
func resolve(ctx context.Context, src secret.Source, ref string) (secret.Text, error) {
	v, err := src.Lookup(ctx, ref)
	if errors.Is(err, secret.ErrNotFound) {
		return secret.Text{}, fault.New(fault.Terminal, "auth_credential_missing", "credential not configured: "+ref)
	}
	if err != nil {
		return secret.Text{}, fault.Wrap(fault.Transient, "auth_credential_resolve", err)
	}
	return v, nil
}

// resolveOptional resolves a reference that may be absent (an empty basic-auth
// username or password). An empty ref or a value the vault lacks yields the empty
// secret; only a backend failure is an error.
func resolveOptional(ctx context.Context, src secret.Source, ref string) (secret.Text, error) {
	if ref == "" {
		return secret.Text{}, nil
	}
	v, err := src.Lookup(ctx, ref)
	if errors.Is(err, secret.ErrNotFound) {
		return secret.Text{}, nil
	}
	if err != nil {
		return secret.Text{}, fault.Wrap(fault.Transient, "auth_credential_resolve", err)
	}
	return v, nil
}

// setHeader validates name and value against the HTTP header grammar before setting
// them, so a credential or prefix carrying CR/LF or another control byte cannot
// inject a header. A malformed header is a terminal fault.
func setHeader(req *http.Request, name, value string) error {
	if !validHeaderName(name) {
		return fault.New(fault.Terminal, "auth_header_name", "invalid header name")
	}
	if !validHeaderValue(value) {
		return fault.New(fault.Terminal, "auth_header_value", "credential contains characters not valid in a header value")
	}
	req.Header.Set(name, value)
	return nil
}

// validHeaderName reports whether s is a non-empty RFC 7230 token.
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !tokenChar[s[i]] {
			return false
		}
	}
	return true
}

// validHeaderValue reports whether s is a valid RFC 7230 field value: visible ASCII,
// spaces, tabs and obs-text (high bytes). Any other control character, CR and LF
// included, is rejected.
func validHeaderValue(s string) bool {
	for i := range len(s) {
		b := s[i]
		if b == '\t' {
			continue
		}
		if b < ' ' || b == 0x7f {
			return false
		}
	}
	return true
}

// tokenChar marks the bytes allowed in an RFC 7230 token (header field name).
var tokenChar = func() [256]bool {
	var t [256]bool
	const special = "!#$%&'*+-.^_`|~"
	for i := 'a'; i <= 'z'; i++ {
		t[i] = true
	}
	for i := 'A'; i <= 'Z'; i++ {
		t[i] = true
	}
	for i := '0'; i <= '9'; i++ {
		t[i] = true
	}
	for i := range len(special) {
		t[special[i]] = true
	}
	return t
}()
