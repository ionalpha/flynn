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
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/integrations/auth"
	"github.com/ionalpha/flynn/internal/integrations/request"
	"github.com/ionalpha/flynn/secret"
)

// maxReadBytes caps how much of a response body the adapter reads, so a hostile
// server cannot stream without end. It exceeds the flow payload cap because the
// flow's MaxPayloadBytes is the policy limit that rejects an over-size body.
const maxReadBytes = 64 << 20 // 64 MiB

// transportDoer adapts the governed request.Transport and an auth.Provider to the
// flow.HTTPDoer port. It resolves each request against the base URL, applies the
// credential, dispatches through the transport (retries, rate limiting, anti-SSRF)
// and decodes the response for a flow step, so the interpreter reaches the network
// only through this path.
type transportDoer struct {
	tr      *request.Transport
	auth    auth.Provider
	secrets secret.Source
	base    *url.URL // parsed base URL; nil when the extension declares none
	// allowedHosts are the hostnames a request may target: the base host plus the
	// extension's egress allow-list. An absolute URL to another host is refused
	// before the credential is applied, so a flow cannot send it elsewhere.
	allowedHosts map[string]bool
}

func newTransportDoer(tr *request.Transport, provider auth.Provider, secrets secret.Source, baseURL string, egressAllow []string) *transportDoer {
	d := &transportDoer{tr: tr, auth: provider, secrets: secrets, allowedHosts: map[string]bool{}}
	if baseURL != "" {
		// A malformed base URL fails each request at resolution, so the doer never
		// holds a half-valid base.
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

// Do performs one request for a flow http step. Every HTTP status, 4xx and 5xx
// included, returns a response so the flow can branch on it; only a transport
// failure with no response is an error.
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

	// The credential is applied last so a flow-supplied header cannot shadow the
	// configured auth.
	if err := d.auth.Apply(ctx, req, d.secrets); err != nil {
		return flow.HTTPResponse{}, err
	}

	resp, derr := d.tr.Do(ctx, req)
	if resp == nil {
		// With no response (network failure, exhausted retries, cancellation) there is
		// no status to inspect, so the transport's classified error stands.
		return flow.HTTPResponse{}, derr
	}
	// A non-retryable 4xx returns a Terminal fault with a readable body, so the flow
	// may branch on the status. Any other fault (retries exhausted, a cancellation)
	// has already drained the body, so the error stands instead of an empty body
	// posing as success.
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

// resolveURL resolves a request URL against the base and confines its host. A
// relative URL is appended to the base path, so a versioned base ".../v1" keeps its
// prefix. An absolute URL must target the base host or the egress allow-list, so a
// flow cannot redirect a credentialed request.
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

// joinPath appends rel to base with one separator, keeping the whole base path where
// plain reference resolution would replace its last segment.
func joinPath(base, rel string) string {
	if rel == "" {
		return base
	}
	if base == "" {
		base = "/"
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(rel, "/")
}

// mergeRawQuery combines the base query with the request's; the request wins on a
// shared key.
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

// decodeBody parses a response body for a flow step. A body labelled JSON, or with
// no text/* type, is parsed so expressions read typed fields (steps.x.body.items). A
// text/* body stays a string, so plain text that looks like a JSON scalar (a bare
// number, true, null) is not retyped. An empty body is null.
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

// firstHeaders flattens an http.Header to one value per name, read by a flow as
// steps.x.headers.
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

// drain reads a little of a response body before closing it, so a keep-alive
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// providerFor builds the auth provider for an extension's auth spec. The credential
// is a name resolved from the vault at call time, so the spec holds no secret. An
// auth type the declarative interpreter does not implement is a terminal error,
// because it belongs to the optional-code port.
func providerFor(a extension.AuthSpec, exchanger auth.TokenExchanger, clk clock.Clock) (auth.Provider, error) {
	cfg, err := authConfig(a)
	if err != nil {
		return nil, err
	}
	var opts []auth.Option
	if cfg.Type == auth.SchemeOAuth2 {
		// The oauth2 scheme fetches its token through the governed transport and
		// tracks expiry on the injected clock.
		opts = append(opts, auth.WithTokenExchanger(exchanger), auth.WithClock(clk))
	}
	return auth.FromConfig(cfg, opts...)
}

// providerForCredential builds the auth provider for a resolved credential. The
// credential's vault reference and, when set, auth type override the extension's,
// so a credential and the request it signs agree on mechanism and secret location.
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
		// The single credential reference maps to the password, so basic auth has no
		// separate username yet. An empty username is valid.
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
		// The integration's credential is the client secret for client_credentials
		// and the refresh token for refresh_token.
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
