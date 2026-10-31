package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// seqTransport serves a scripted sequence of responses, repeating the last one.
type seqTransport struct {
	responses []*http.Response
	calls     int
}

func (s *seqTransport) RoundTrip(*http.Request) (*http.Response, error) {
	i := min(s.calls, len(s.responses)-1)
	s.calls++
	return s.responses[i], nil
}

func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func clientWith(rt http.RoundTripper) *Client {
	return New("prov", "https://api.example.com", nil, &http.Client{Transport: rt})
}

func post(t *testing.T, c *Client) error {
	t.Helper()
	var out struct{}
	return c.PostJSON(context.Background(), "/v1/x", map[string]string{"a": "b"}, &out)
}

// TestQuotaClassifierUnion pins the one retry-vs-fail decision for every
// provider: both OpenAI's structured insufficient_quota signal and Anthropic's
// credit/billing message phrasing turn a 429 terminal, while a plain rate-limit
// 429 and a 500 stay transient and a 400 is terminal.
func TestQuotaClassifierUnion(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   fault.Class
	}{
		{"openai insufficient_quota type", 429, `{"error":{"type":"insufficient_quota","message":"x"}}`, fault.Terminal},
		{"openai insufficient_quota code", 429, `{"error":{"code":"insufficient_quota","message":"x"}}`, fault.Terminal},
		{"anthropic credit message", 429, `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`, fault.Terminal},
		{"billing message", 429, `{"error":{"message":"billing problem"}}`, fault.Terminal},
		{"plain rate limit stays transient", 429, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, fault.Transient},
		{"500 transient", 500, `{"error":{"message":"internal"}}`, fault.Transient},
		{"400 terminal", 400, `{"error":{"message":"bad request"}}`, fault.Terminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := statusError("prov", tc.status, []byte(tc.body))
			if got := fault.Classify(err); got != tc.want {
				t.Fatalf("class = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestPostJSONRetriesTransientThenSucceeds(t *testing.T) {
	rt := &seqTransport{responses: []*http.Response{
		resp(500, `{"error":{"message":"internal"}}`), //nolint:bodyclose // PostJSON closes the response body
		resp(200, `{"ok":true}`),                      //nolint:bodyclose // PostJSON closes the response body
	}}
	c := clientWith(rt)
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.PostJSON(context.Background(), "/v1/x", map[string]string{}, &out); err != nil {
		t.Fatalf("PostJSON after transient 500: %v", err)
	}
	if !out.OK || rt.calls != 2 {
		t.Fatalf("out=%+v calls=%d, want decoded body on attempt 2", out, rt.calls)
	}
}

func TestPostJSONCapsResponseBody(t *testing.T) {
	huge := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(io.LimitReader(zeros{}, MaxResponseBytes+2)),
		Header:     make(http.Header),
	}
	err := post(t, clientWith(&seqTransport{responses: []*http.Response{huge}}))
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("an oversized body must be rejected, got %v", err)
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("oversize must be terminal, got %v", fault.Classify(err))
	}
}

func TestPostJSONDecodeErrorIsTerminal(t *testing.T) {
	err := post(t, clientWith(&seqTransport{responses: []*http.Response{resp(200, `{`)}})) //nolint:bodyclose // PostJSON closes the response body
	if err == nil || fault.Classify(err) != fault.Terminal {
		t.Fatalf("malformed 2xx body must be terminal, got %v", err)
	}
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestDefaultClientLoopbackVsHosted(t *testing.T) {
	if !isLoopback("http://127.0.0.1:8080/v1") || !isLoopback("http://localhost:11434") {
		t.Fatal("loopback endpoints must be detected")
	}
	if isLoopback("https://api.example.com") || isLoopback("http://10.0.0.5") {
		t.Fatal("remote endpoints must not be treated as loopback")
	}
	// The hosted default carries a netguard-controlled transport; the loopback
	// default is a plain client (netguard would refuse to dial the local host).
	if defaultClient("https://api.example.com").Transport == nil {
		t.Fatal("hosted default must install the guarded transport")
	}
	if defaultClient("http://127.0.0.1:9999").Transport != nil {
		t.Fatal("loopback default must stay a plain client")
	}
}

// TestContextOverflowIsTyped pins the classification of a refused-for-length response, in the words
// of each server that sends one, so a caller matches it with errors.Is and never parses text. The
// refusals are terminal, print as the provider's own message, and a status that cannot be one
// (a 500, a 429, a 401) is left alone even when its text mentions the context.
func TestContextOverflowIsTyped(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"llama.cpp type", 400, `{"error":{"code":400,"message":"request (149849 tokens) exceeds the available context size (115456 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":149849,"n_ctx":115456}}`, true},
		{"llama.cpp message only", 400, `{"error":{"message":"the request exceeds the available context size"}}`, true},
		{"vllm message", 400, `{"object":"error","message":"This model's maximum context length is 4096 tokens. However, you requested 5000 tokens","type":"BadRequestError","code":400}`, true},
		{"openai code", 400, `{"error":{"code":"context_length_exceeded","message":"too long","type":"invalid_request_error"}}`, true},
		{"payload too large", 413, `{"error":{"code":"context_length_exceeded","message":"too long"}}`, true},
		{"other 400", 400, `{"error":{"message":"bad request"}}`, false},
		{"server error naming the context", 500, `{"error":{"message":"exceeds the available context size"}}`, false},
		{"rate limit naming the context", 429, `{"error":{"code":"context_length_exceeded","message":"slow down"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := post(t, clientWith(&seqTransport{responses: []*http.Response{resp(tc.status, tc.body)}})) //nolint:bodyclose // PostJSON closes the response body
			if got := errors.Is(err, llm.ErrContextOverflow); got != tc.want {
				t.Fatalf("errors.Is(err, ErrContextOverflow) = %v, want %v for %v", got, tc.want, err)
			}
			if tc.want && fault.Classify(err) != fault.Terminal {
				t.Fatalf("an overflow refusal must be terminal, got %v", fault.Classify(err))
			}
		})
	}
}

// TestContextOverflowKeepsMessage proves classifying the refusal does not change what a user reads.
func TestContextOverflowKeepsMessage(t *testing.T) {
	body := `{"error":{"message":"request (9 tokens) exceeds the available context size (4 tokens)","type":"exceed_context_size_error"}}`
	err := post(t, clientWith(&seqTransport{responses: []*http.Response{resp(400, body)}})) //nolint:bodyclose // PostJSON closes the response body
	want := "prov_status: prov: HTTP 400: request (9 tokens) exceeds the available context size (4 tokens)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
