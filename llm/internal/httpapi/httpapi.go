// Package httpapi is the shared HTTP core for the hosted-provider adapters: one governed
// POST-JSON pipeline (netguard dialing, capped reads, Retry-After aware retries) and one
// quota classifier, so every adapter makes the same retry decision and a hardening fix
// lands once.
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
	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/secret"
)

const (
	// DefaultTimeout bounds one HTTP attempt. A non-streaming model turn can run for
	// minutes before the first byte.
	DefaultTimeout = 10 * time.Minute
	// MaxResponseBytes caps the body read, so an endpoint reached through a base-URL
	// override cannot exhaust memory. A real response is far smaller, bounded by the
	// output-token ceiling.
	MaxResponseBytes = 32 << 20
)

// Client posts JSON requests to one provider API. Build one per adapter.
type Client struct {
	name      string
	baseURL   string
	headers   func(http.Header)
	transport *request.Transport
}

// New builds a Client for the provider name, used as the fault-code and message prefix.
// headers stamps authentication on every request. A nil httpClient picks a plain client
// for loopback and netguard for anything else, so a hosted call cannot be steered at a
// private, loopback or cloud-metadata address.
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

// PostJSON marshals in, posts it to path, and decodes a 2xx body into out. The
// transport retries network faults and 408, 429 and 5xx with backoff, honouring
// Retry-After, and the read is capped at [MaxResponseBytes]. Encode, decode and client
// errors are terminal; network and server errors are transient, except a quota 429 (see
// statusError).
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
		// No response: keep the transport's classification under the provider's code.
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
		return statusError(c.name, resp.StatusCode, raw, credentials(req.Header)...)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fault.Wrap(fault.Terminal, c.name+"_decode", err)
	}
	return nil
}

// statusError maps an HTTP error response to a fault: rate limits and server errors are
// transient, client errors terminal. A 429 for an exhausted quota or billing problem is
// terminal, so a worker does not retry for hours against an account that cannot succeed.
// It is the one quota classifier for every provider, the union of their signals: OpenAI's
// "insufficient_quota" type or code, with a quota or billing message as fallback, and
// Anthropic's message ("credit balance is too low", billing).
//
// Each of secrets is replaced by [secret.Redacted] in the message before it goes into
// the fault, because a provider or proxy can echo the key it refused and the fault
// text reaches the terminal and any log a dev run is saved to.
func statusError(name string, code int, body []byte, secrets ...string) error {
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
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, secret.Redacted)
		}
	}
	quota := e.Error.Type == "insufficient_quota" || e.Error.Code == "insufficient_quota" ||
		text.ContainsAny(strings.ToLower(msg), "credit", "billing", "quota")
	full := fmt.Sprintf("%s: HTTP %d: %s", name, code, msg)
	if isContextOverflow(code, e.Error.Type, e.Error.Code, msg) {
		return fault.Wrap(fault.Terminal, name+"_status", overflowError(full))
	}
	return fault.New(llm.RetryClass(code, quota), name+"_status", full)
}

// credentialHeaders are the request headers a provider adapter puts its key in.
var credentialHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"}

// credentials returns the key values h carries, with a bearer scheme stripped, so
// [statusError] can redact them whichever form a provider echoes back.
func credentials(h http.Header) []string {
	var out []string
	for _, name := range credentialHeaders {
		for _, v := range h.Values(name) {
			out = append(out, v)
			if scheme, tok, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "bearer") {
				out = append(out, strings.TrimSpace(tok))
			}
		}
	}
	return out
}

// overflowError is a refused-for-length response carrying the provider's own message,
// so it prints that message and still matches [llm.ErrContextOverflow].
type overflowError string

func (e overflowError) Error() string { return string(e) }

func (overflowError) Is(target error) bool { return target == llm.ErrContextOverflow }

// isContextOverflow reports whether an error response says the prompt exceeded the
// context window. Each server words it differently, so the signals are listed once:
// llama.cpp's exceed_context_size_error and "exceeds the available context size", vLLM's
// "maximum context length", OpenAI's context_length_exceeded. Only a 400 or 413 can be
// one; a 5xx or 429 that mentions context keeps its retry class.
func isContextOverflow(status int, errType, errCode, msg string) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge {
		return false
	}
	if errType == "exceed_context_size_error" || errCode == "context_length_exceeded" {
		return true
	}
	return text.ContainsAny(strings.ToLower(msg), "exceeds the available context size", "maximum context length")
}

// defaultClient picks the client when no caller overrode it. Loopback stays plain,
// because netguard refuses to dial it. Neither sets a response-header timeout, because
// a non-streaming model call is silent for the whole generation.
func defaultClient(baseURL string) *http.Client {
	if isLoopback(baseURL) {
		return &http.Client{Timeout: DefaultTimeout}
	}
	dialer := netguard.Dialer(netguard.PublicOnly())
	return &http.Client{
		Timeout: DefaultTimeout,
		Transport: &http.Transport{
			// No proxy, because a proxy would carry the request past the dial-time
			// address guard.
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}
}

// isLoopback reports whether the base URL targets the local machine, matching the
// loopback branch of llm.SafeBaseURL.
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
