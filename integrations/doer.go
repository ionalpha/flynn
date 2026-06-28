package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/flow"
	"github.com/ionalpha/flynn/integrations/auth"
	"github.com/ionalpha/flynn/integrations/request"
	"github.com/ionalpha/flynn/secret"
)

// maxReadBytes is a hard ceiling on how much of a response body the adapter reads
// into memory, a safety net against an unbounded response. It is deliberately larger
// than the flow payload cap: the flow's MaxPayloadBytes is the policy limit that
// rejects an over-size body, while this only stops a hostile server from streaming
// without end.
const maxReadBytes = 64 << 20 // 64 MiB

// transportDoer adapts the governed request.Transport and an auth.Provider to the
// flow.HTTPDoer port. It resolves each flow request against the integration's base
// URL, applies the configured credential, dispatches through the transport (retries,
// rate limiting, anti-SSRF), and decodes the response into the value shape a flow
// step consumes. The interpreter therefore reaches the network only through this one
// governed path.
type transportDoer struct {
	tr      *request.Transport
	auth    auth.Provider
	secrets secret.Source
	base    *url.URL // parsed base URL; nil when the extension declares none
	// allowedHosts is the set of hostnames a request may target. It always contains
	// the base host and is extended by the extension's declared egress allow-list. An
	// absolute request URL to any other host is refused before the credential is
	// applied, so a flow cannot be made to send the integration's credential to an
	// arbitrary host.
	allowedHosts map[string]bool
}

func newTransportDoer(tr *request.Transport, provider auth.Provider, secrets secret.Source, baseURL string, egressAllow []string) *transportDoer {
	d := &transportDoer{tr: tr, auth: provider, secrets: secrets, allowedHosts: map[string]bool{}}
	if baseURL != "" {
		// A malformed base URL surfaces later as a per-request resolution error, so
		// the doer never holds a half-valid base.
		if u, err := url.Parse(baseURL); err == nil {
			d.base = u
			if h := strings.ToLower(u.Hostname()); h != "" {
				d.allowedHosts[h] = true
			}
		}
	}
	for _, h := range egressAllow {
		if h != "" {
			d.allowedHosts[strings.ToLower(h)] = true
		}
	}
	return d
}

// Do performs one request for a flow http step. A response is returned for every
// HTTP status (including 4xx/5xx) so the flow can branch on status.body; only a
// transport-level failure with no response surfaces as an error.
func (d *transportDoer) Do(ctx context.Context, r flow.HTTPRequest) (flow.HTTPResponse, error) {
	u, err := d.resolveURL(r.URL)
	if err != nil {
		return flow.HTTPResponse{}, err
	}
	if len(r.Query) > 0 {
		q := u.Query()
		for k, v := range r.Query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	method := r.Method
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return flow.HTTPResponse{}, fault.Wrap(fault.Terminal, "integration_request", err)
	}
	if len(r.Body) > 0 {
		// GetBody lets the transport replay the body on a retry.
		raw := r.Body
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		req.ContentLength = int64(len(raw))
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	if len(r.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Apply the credential last so a flow-supplied header cannot shadow the auth the
	// integration is configured to send.
	if err := d.auth.Apply(ctx, req, d.secrets); err != nil {
		return flow.HTTPResponse{}, err
	}

	resp, derr := d.tr.Do(ctx, req)
	if resp == nil {
		// No response at all (network failure, exhausted retries, cancellation): the
		// flow cannot inspect a status, so the transport's classified error stands.
		return flow.HTTPResponse{}, derr
	}
	// A non-retryable status (a 4xx) returns a Terminal fault with a readable body, so
	// the flow may branch on the status. Any other fault (a retryable status exhausted
	// over retries, a cancellation) leaves the body already drained, so the error
	// stands rather than presenting an empty body as success.
	if derr != nil && fault.Classify(derr) != fault.Terminal {
		drain(resp)
		return flow.HTTPResponse{}, derr
	}
	defer drain(resp)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return flow.HTTPResponse{}, fault.Wrap(fault.Transient, "integration_read", err)
	}
	return flow.HTTPResponse{
		Status:  resp.StatusCode,
		Headers: firstHeaders(resp.Header),
		Body:    decodeBody(raw, resp.Header.Get("Content-Type")),
		Raw:     raw,
	}, nil
}

// resolveURL resolves a flow request URL against the base and confines its host. A
// relative URL is appended to the base path (the base path is preserved, not
// dropped, so a versioned base like ".../v1" keeps its prefix). An absolute URL is
// allowed only when its host is the base host or in the egress allow-list, so a flow
// cannot redirect a credentialed request to an arbitrary host.
func (d *transportDoer) resolveURL(raw string) (*url.URL, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "integration_url", err)
	}
	if ref.IsAbs() {
		if !d.allowedHosts[strings.ToLower(ref.Hostname())] {
			return nil, fault.New(fault.Terminal, "integration_egress",
				"integration: request host "+ref.Host+" is not the base host or an allowed egress host")
		}
		return ref, nil
	}
	if d.base == nil {
		return nil, fault.New(fault.Terminal, "integration_url", "integration: relative url but the extension declares no base URL")
	}
	out := *d.base
	out.Path = joinPath(d.base.Path, ref.Path)
	out.RawPath = ""
	out.RawQuery = mergeRawQuery(d.base.RawQuery, ref.RawQuery)
	out.Fragment = ref.Fragment
	return &out, nil
}

// joinPath appends a relative path to a base path with exactly one separator,
// preserving the whole base path rather than treating its last segment as a file to
// replace (the behaviour plain reference resolution would give).
func joinPath(base, rel string) string {
	if rel == "" {
		return base
	}
	if base == "" {
		base = "/"
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(rel, "/")
}

// mergeRawQuery combines the base URL's query with the request's, the request taking
// precedence on a shared key.
func mergeRawQuery(base, ref string) string {
	if base == "" {
		return ref
	}
	if ref == "" {
		return base
	}
	bv, _ := url.ParseQuery(base)
	rv, _ := url.ParseQuery(ref)
	for k, vs := range rv {
		bv[k] = vs
	}
	return bv.Encode()
}

// decodeBody parses a response body into the value a flow step consumes. A body the
// server labels as JSON (or carries no text/* type) is parsed so expressions read
// typed fields (steps.x.body.items); a body the server labels text/* is kept as a
// string, so a plain-text payload that happens to look like a JSON scalar (a bare
// number, true, null) is not silently retyped. An empty body is null.
func decodeBody(raw []byte, contentType string) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/") {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return string(raw)
}

// firstHeaders flattens an http.Header to one value per name, the shape a flow reads
// as steps.x.headers.
func firstHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// drain closes a response body, reading a little of it first so a keep-alive
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// providerFor builds the auth provider for an extension's auth spec, mapping the
// manifest's declarative auth onto the transport's credential model. The credential
// is referenced by name and resolved from the vault at call time; the spec carries
// no secret. An auth type the declarative interpreter does not implement (oauth2 and
// custom protocols) is a terminal error here, since it belongs to the optional-code
// port rather than this surface.
func providerFor(a extension.AuthSpec, exchanger auth.TokenExchanger, clk clock.Clock) (auth.Provider, error) {
	cfg, err := authConfig(a)
	if err != nil {
		return nil, err
	}
	var opts []auth.Option
	if cfg.Type == auth.SchemeOAuth2 {
		// The oauth2 scheme obtains its token through the governed transport and tracks
		// expiry against the injected clock.
		opts = append(opts, auth.WithTokenExchanger(exchanger), auth.WithClock(clk))
	}
	return auth.FromConfig(cfg, opts...)
}

// providerForCredential builds the auth provider for a resolved credential. The
// credential supplies the effective vault reference (where its secret lives) and,
// when set, the auth type, overriding the extension's defaults so a credential and
// the request it signs always agree on the mechanism and the secret location.
func providerForCredential(a extension.AuthSpec, cred credential.Credential, exchanger auth.TokenExchanger, clk clock.Clock) (auth.Provider, error) {
	a.CredentialRef = cred.Ref()
	if cred.Spec.AuthType != "" {
		a.Type = cred.Spec.AuthType
	}
	return providerFor(a, exchanger, clk)
}

func authConfig(a extension.AuthSpec) (auth.Config, error) {
	switch a.Type {
	case "", "none":
		return auth.Config{Type: auth.SchemeNone}, nil
	case "bearer":
		return auth.Config{Type: auth.SchemeBearer, TokenRef: a.CredentialRef}, nil
	case "api_key":
		in := auth.InHeader
		if a.In == string(auth.InQuery) {
			in = auth.InQuery
		}
		return auth.Config{Type: auth.SchemeAPIKey, TokenRef: a.CredentialRef, Param: a.Name, In: in}, nil
	case "basic":
		// The single credential reference maps to the password; the named, multi-key
		// credential model that gives basic auth a separate username is a later
		// addition. An empty username is valid.
		return auth.Config{Type: auth.SchemeBasic, PasswordRef: a.CredentialRef}, nil
	case "oauth2":
		if a.OAuth2 == nil {
			return auth.Config{}, fault.New(fault.Terminal, "integration_auth", "integration: oauth2 auth needs an oauth2 block")
		}
		grant := a.OAuth2.Grant
		if grant == "" {
			grant = auth.GrantClientCredentials
		}
		cfg := auth.Config{
			Type:     auth.SchemeOAuth2,
			TokenURL: a.OAuth2.TokenURL,
			ClientID: a.OAuth2.ClientID,
			Grant:    grant,
			Scopes:   a.OAuth2.Scopes,
		}
		// The integration's credential supplies the oauth2 secret: the client secret
		// for the client_credentials grant, the refresh token for the refresh_token
		// grant.
		if grant == auth.GrantRefreshToken {
			cfg.RefreshTokenRef = a.CredentialRef
		} else {
			cfg.ClientSecretRef = a.CredentialRef
		}
		return cfg, nil
	default:
		return auth.Config{}, fault.New(fault.Terminal, "integration_auth",
			"integration: auth type "+a.Type+" is not supported by the declarative interpreter; use the code port")
	}
}
