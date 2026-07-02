// Package httpapi is the shared HTTP core for the hosted-provider adapters: one
// governed POST-JSON pipeline (netguard-dialed by default, capped response read,
// retry with Retry-After through the integrations transport) and one quota
// classifier, so every adapter makes the same retry-vs-fail decision and a
// hardening fix lands once instead of per provider. Adapters stay thin wire-format
// mappers: they build a request struct, call PostJSON, and decode their response
// type.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/integrations/request"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/netguard"
)

const (
	// DefaultTimeout bounds one whole HTTP attempt. It is generous because a
	// single non-streaming model turn can legitimately run for minutes before
	// the first response byte arrives.
	DefaultTimeout = 10 * time.Minute
	// MaxResponseBytes caps the response-body read, so a hostile or broken
	// endpoint (reachable via a base-URL override) cannot exhaust memory with an
	// unbounded body. Far above any real model response, which is bounded by the
	// output-token ceiling.
	MaxResponseBytes = 32 << 20
)

// Client posts JSON requests to one provider API. Build it once per adapter.
type Client struct {
	name      string
	baseURL   string
	headers   func(http.Header)
	transport *request.Transport
}

// New builds a Client for the provider named name (the fault-code and message
// prefix), rooted at baseURL. headers stamps per-provider authentication onto
// every request. httpClient overrides the underlying HTTP client (tests inject a
// mock transport); nil selects the default for the endpoint: loopback (a local
// model server) gets a plain client, anything else dials through netguard so a
// hosted call can never be steered at a private, loopback, or cloud-metadata
// address.
func New(name, baseURL string, headers func(http.Header), httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultClient(baseURL)
	}
	return &Client{
		name:      name,
		baseURL:   baseURL,
		headers:   headers,
		transport: request.New(request.WithDoer(httpClient)),
	}
}

// PostJSON marshals in, posts it to path, and decodes a 2xx body into out.
// Transient failures (network faults, 408/429/5xx) are retried by the transport
// with backoff, honouring Retry-After; the response read is capped at
// MaxResponseBytes. Every error is fault-classified: encode/decode and client
// errors terminal, network and server errors transient, with the 429 quota
// exception decided by the shared classifier in statusError.
func (c *Client) PostJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fault.Wrap(fault.Terminal, c.name+"_encode", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fault.Wrap(fault.Terminal, c.name+"_request", err)
	}
	req.Header.Set("content-type", "application/json")
	if c.headers != nil {
		c.headers(req.Header)
	}

	resp, err := c.transport.Do(ctx, req)
	if resp == nil {
		// No response at all: surface the transport's classification (transient
		// network fault, cancelled context) under the provider's code.
		return fault.Wrap(fault.Classify(err), c.name+"_http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return fault.Wrap(fault.Transient, c.name+"_read", err)
	}
	if int64(len(raw)) > MaxResponseBytes {
		return fault.New(fault.Terminal, c.name+"_read",
			fmt.Sprintf("%s: response exceeds the %d-byte cap; rejected", c.name, int64(MaxResponseBytes)))
	}
	if resp.StatusCode/100 != 2 {
		return statusError(c.name, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fault.Wrap(fault.Terminal, c.name+"_decode", err)
	}
	return nil
}

// statusError maps an HTTP error response to a fault-classified error: rate
// limits and server errors are transient so the worker retries; client errors are
// terminal. The exception that matters is a 429 that is really an exhausted quota
// or billing problem: that is permanent and must fail fast rather than retry for
// hours against an account that cannot succeed. This is the one quota classifier
// for every provider, the union of their signals: OpenAI marks the case with the
// error type or code "insufficient_quota" (message mentioning quota/billing as a
// fallback); Anthropic phrases it in the message ("credit balance is too low",
// billing). One list means retry-vs-fail can never drift between adapters again.
func statusError(name string, code int, body []byte) error {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = string(body)
	}
	quota := e.Error.Type == "insufficient_quota" || e.Error.Code == "insufficient_quota" ||
		containsAny(strings.ToLower(msg), "credit", "billing", "quota")
	return fault.New(llm.RetryClass(code, quota), name+"_status", fmt.Sprintf("%s: HTTP %d: %s", name, code, msg))
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// defaultClient selects the client for an endpoint no caller overrode. Loopback
// stays plain: the traffic never leaves the machine, and the netguard policy
// would (correctly, for remote calls) refuse to dial it. Everything else dials
// through netguard. Neither sets a response-header timeout: a non-streaming model
// call holds the connection silently for as long as the generation takes.
func defaultClient(baseURL string) *http.Client {
	if isLoopback(baseURL) {
		return &http.Client{Timeout: DefaultTimeout}
	}
	dialer := netguard.Dialer(netguard.PublicOnly())
	return &http.Client{
		Timeout: DefaultTimeout,
		Transport: &http.Transport{
			// No proxy: a proxy would carry the request past the dial-time
			// address guard, the same reasoning as netguard.Client.
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}
}

// isLoopback reports whether the base URL targets the local machine, mirroring
// the loopback branch of llm.SafeBaseURL.
func isLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
