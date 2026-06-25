// Package auth applies an integration's credentials to an outbound HTTP request.
// It is the pluggable authentication half of the integration framework: a Config
// (plain data, part of an integration spec) selects a Provider, and every surface
// that calls a remote API (the API tool, the scraper) authenticates the same way,
// by calling Apply before handing the request to the shared request transport.
//
// Credentials are never written into a spec. A Config names the auth scheme and
// the vault references to resolve, and Apply looks each reference up through a
// secret.Source (the vault boundary) at call time. The resolved value is a
// secret.Text and is exposed exactly once, at the single point where it is written
// onto the request, so a credential never lands in a logged config, an event, or
// the spec itself.
//
// Apply is defensive about the data it writes. The header name and value it
// produces are validated against the HTTP grammar before they are set, so a hostile
// or malformed credential, prefix, or parameter name cannot smuggle CR/LF and
// inject a second header. Query-placed keys go through url.Values, which
// percent-encodes them, so the same injection is structurally impossible there.
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/secret"
)

// Scheme names an authentication mechanism. It is the discriminant of a Config:
// the value an integration spec carries to say how its requests are signed.
type Scheme string

const (
	// SchemeNone applies no credentials. It is the explicit "public API" choice,
	// distinct from an unset scheme, and the zero value.
	SchemeNone Scheme = "none"
	// SchemeBasic sends RFC 7617 HTTP Basic credentials in the Authorization header.
	SchemeBasic Scheme = "basic"
	// SchemeBearer sends a token as "Authorization: Bearer <token>".
	SchemeBearer Scheme = "bearer"
	// SchemeAPIKey sends a key in a named header or query parameter, with an
	// optional value prefix.
	SchemeAPIKey Scheme = "api_key"
)

// Placement is where SchemeAPIKey puts the key.
type Placement string

const (
	// InHeader places the key in a request header (the default).
	InHeader Placement = "header"
	// InQuery places the key in a URL query parameter.
	InQuery Placement = "query"
)

// Config is the data form of an integration's auth: the scheme plus the vault
// references and placement it needs. It carries no secret values, only the names
// to resolve them by, so it is safe to serialize, log, and store in a spec. The
// zero Config is SchemeNone (apply nothing).
type Config struct {
	Type Scheme `json:"type,omitempty"`

	// TokenRef is the vault reference for the credential used by SchemeBearer and
	// SchemeAPIKey.
	TokenRef string `json:"token_ref,omitempty"`

	// UsernameRef and PasswordRef are the vault references for SchemeBasic. Either
	// may be empty (an empty username or password is sent), but not both: a basic
	// config that resolves nothing is an error.
	UsernameRef string `json:"username_ref,omitempty"`
	PasswordRef string `json:"password_ref,omitempty"`

	// In selects header or query placement for SchemeAPIKey. Empty means InHeader.
	In Placement `json:"in,omitempty"`
	// Param is the header or query-parameter name for SchemeAPIKey (e.g.
	// "X-API-Key", "api_key"). Required for SchemeAPIKey.
	Param string `json:"param,omitempty"`
	// Prefix is an optional literal prepended to the SchemeAPIKey value (e.g.
	// "Token ", "Bearer "). It is part of the wire value, not a secret.
	Prefix string `json:"prefix,omitempty"`
}

// Provider applies a single auth scheme to a request. It is the pluggable unit of
// the framework: FromConfig builds one from a Config, and a surface calls Apply on
// every outbound request before dispatching it through the transport.
type Provider interface {
	// Apply resolves the credentials this provider needs from src and writes them
	// onto req. A missing credential (secret.ErrNotFound) is a terminal fault: the
	// integration is configured to need it and cannot proceed without it. Apply
	// never writes a header it could not validate.
	Apply(ctx context.Context, req *http.Request, src secret.Source) error
	// Scheme reports the mechanism this provider implements, for audit and
	// introspection.
	Scheme() Scheme
}

// FromConfig builds the Provider a Config selects, validating that the config
// carries the references its scheme requires. An unknown scheme, or a scheme
// missing a required field, is a terminal configuration fault. The zero Config and
// SchemeNone both yield the no-op provider.
func FromConfig(c Config) (Provider, error) {
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
	default:
		return nil, fault.New(fault.Terminal, "auth_config", "unknown auth scheme: "+string(c.Type))
	}
}

// none applies no credentials.
type none struct{}

func (none) Apply(context.Context, *http.Request, secret.Source) error { return nil }
func (none) Scheme() Scheme                                            { return SchemeNone }

// basic sends RFC 7617 Basic credentials. The base64 of "user:pass" is always
// header-safe, so the value needs no validation.
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

// bearer sends a token as an Authorization: Bearer header.
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

// apiKey sends a key in a named header or query parameter, with an optional prefix.
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
		// url.Values.Encode percent-encodes name and value, so the key cannot alter
		// the URL structure however hostile its bytes are.
		q := req.URL.Query()
		q.Set(a.param, value)
		req.URL.RawQuery = q.Encode()
		return nil
	}
	return setHeader(req, a.param, value)
}

func (a apiKey) Scheme() Scheme { return SchemeAPIKey }

// resolve looks a required reference up in the vault. A missing value is terminal:
// the integration declared it needs this credential. A backend failure (a locked
// keychain, an unreachable vault) is transient, so a caller may retry it.
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

// resolveOptional resolves a reference that may be intentionally absent (an empty
// basic-auth username or password). An empty ref, or one the vault does not hold,
// yields the empty secret without error; only a backend failure is reported.
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

// setHeader validates name and value against the HTTP header grammar before
// setting them, so a credential or prefix carrying CR/LF (or any control byte)
// cannot inject an extra header. It returns a terminal fault rather than writing a
// malformed header.
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

// validHeaderName reports whether s is a non-empty RFC 7230 token (the grammar for
// a header field name).
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

// validHeaderValue reports whether s is a valid RFC 7230 header field value: it may
// hold visible ASCII, spaces, tabs, and obs-text (high bytes), but no other control
// characters, which is what rejects an embedded CR or LF.
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
