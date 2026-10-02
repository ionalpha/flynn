package embed_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm/embed"
	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/secret"
)

// The port this package exists to fill. Recall names no provider and this adapter
// names no recall; the assertion is the only place the two meet.
var _ hybrid.Embedder = (*embed.Client)(nil)

// scriptedTransport answers each request with the next scripted reply and keeps
// what it was sent, so a test can assert on the batching as well as the result.
type scriptedTransport struct {
	replies  []string
	status   int
	requests []embedRequest
	calls    int
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	url   string
	auth  string
}

func (s *scriptedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var got embedRequest
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}
	got.url, got.auth = r.URL.String(), r.Header.Get("authorization")
	s.requests = append(s.requests, got)

	reply := `{"data":[]}`
	if s.calls < len(s.replies) {
		reply = s.replies[s.calls]
	}
	s.calls++
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
}

func clientWith(s *scriptedTransport, opts ...embed.Option) *embed.Client {
	opts = append([]embed.Option{embed.WithHTTPClient(&http.Client{Transport: s})}, opts...)
	return embed.New(secret.New("test-key"), opts...)
}

// TestEmbedPairsVectorsByIndexNotArrival is the one that matters most: a ranking
// built on vectors paired with the wrong text is a recall that works and returns
// the wrong memories, which nothing downstream can detect.
func TestEmbedPairsVectorsByIndexNotArrival(t *testing.T) {
	s := &scriptedTransport{replies: []string{
		`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`,
	}}
	c := clientWith(s, embed.WithModel("text-embedding-3-large"))

	got, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][1] != 1 {
		t.Fatalf("vectors = %v, want them back in the order the texts went out", got)
	}
	req := s.requests[0]
	if req.Model != "text-embedding-3-large" || strings.Join(req.Input, ",") != "first,second" {
		t.Errorf("request = %+v, want the configured model and both texts", req)
	}
	if !strings.HasSuffix(req.url, "/embeddings") || req.auth != "Bearer test-key" {
		t.Errorf("request went to %q with auth %q", req.url, req.auth)
	}
	if c.Model() != "text-embedding-3-large" {
		t.Errorf("Model() = %q, want what an operator was told they configured", c.Model())
	}
}

// A corpus goes out in batches, and comes back as one ordered result.
func TestEmbedBatchesAndKeepsCorpusOrder(t *testing.T) {
	s := &scriptedTransport{replies: []string{
		`{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]}]}`,
		`{"data":[{"index":0,"embedding":[3]},{"index":1,"embedding":[4]}]}`,
		`{"data":[{"index":0,"embedding":[5]}]}`,
	}}
	c := clientWith(s, embed.WithBatch(2))

	got, err := c.Embed(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("vectors = %d, want 5", len(got))
	}
	for i, v := range got {
		if len(v) != 1 || v[0] != float32(i+1) {
			t.Fatalf("vector %d = %v, want the %dth vector of the corpus", i, v, i+1)
		}
	}
	if s.calls != 3 {
		t.Errorf("calls = %d, want 5 texts in batches of 2 to be three requests", s.calls)
	}
	if strings.Join(s.requests[2].Input, ",") != "e" {
		t.Errorf("last batch = %v, want the remainder", s.requests[2].Input)
	}
}

// Embedding nothing is not a request. An endpoint asked for no vectors can only
// fail, and a recall over an empty candidate set has nothing to rank anyway.
func TestEmbedNothingMakesNoCall(t *testing.T) {
	s := &scriptedTransport{}
	got, err := clientWith(s).Embed(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("Embed(nil) = %v, %v, want no vectors and no error", got, err)
	}
	if s.calls != 0 {
		t.Fatalf("calls = %d, want none", s.calls)
	}
}

// A reply that does not answer the question asked is refused rather than fused
// into a ranking. Each of these leaves at least one text without its own vector,
// and the caller's fallback is a lexical order, which is a worse ranking and not a
// wrong one.
func TestEmbedRefusesAMisalignedReply(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{name: "fewer vectors than texts", reply: `{"data":[{"index":0,"embedding":[1]}]}`},
		{name: "more vectors than texts", reply: `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]},{"index":2,"embedding":[3]}]}`},
		{name: "an index outside the batch", reply: `{"data":[{"index":0,"embedding":[1]},{"index":9,"embedding":[2]}]}`},
		{name: "a negative index", reply: `{"data":[{"index":0,"embedding":[1]},{"index":-1,"embedding":[2]}]}`},
		{name: "the same text twice", reply: `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := clientWith(&scriptedTransport{replies: []string{tt.reply}})
			got, err := c.Embed(context.Background(), []string{"a", "b"})
			if err == nil {
				t.Fatalf("Embed = %v, want the reply refused", got)
			}
			if fault.Classify(err) != fault.Terminal {
				t.Fatalf("err = %v (%s), want terminal: retrying will not realign it", err, fault.Classify(err))
			}
		})
	}
}

// An endpoint that refuses is the caller's problem to fall back from, so the error
// reaches it rather than becoming an empty vector set that reads as a corpus with
// no meaning in it.
func TestEmbedReportsAnEndpointFailure(t *testing.T) {
	s := &scriptedTransport{status: http.StatusUnauthorized, replies: []string{`{"error":{"message":"no key"}}`}}
	if _, err := clientWith(s).Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("Embed over a 401 = nil, want the failure")
	}
}

// A keyless endpoint (a loopback model server) sends no authorization header, and
// an override that would carry a key over plaintext to a remote host is refused
// while the secure default stands.
func TestEmbedKeylessAndUnsafeBaseURL(t *testing.T) {
	s := &scriptedTransport{replies: []string{`{"data":[{"index":0,"embedding":[1]}]}`}}
	c := embed.New(secret.Text{},
		embed.WithHTTPClient(&http.Client{Transport: s}),
		embed.WithBaseURL("http://127.0.0.1:8080/v1"),
		embed.WithBatch(0),
	)
	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := s.requests[0]; got.auth != "" || !strings.HasPrefix(got.url, "http://127.0.0.1:8080/v1") {
		t.Fatalf("request = %q with auth %q, want the loopback endpoint and no header", got.url, got.auth)
	}

	unsafe := &scriptedTransport{replies: []string{`{"data":[{"index":0,"embedding":[1]}]}`}}
	c = embed.New(secret.New("test-key"),
		embed.WithHTTPClient(&http.Client{Transport: unsafe}),
		embed.WithBaseURL("http://example.com/v1"),
		embed.WithModel(""),
	)
	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := unsafe.requests[0].url; !strings.HasPrefix(got, "https://api.openai.com/") {
		t.Fatalf("request went to %q, want the plaintext override refused and the default kept", got)
	}
	if c.Model() != embed.DefaultModel {
		t.Fatalf("Model() = %q, want the default restored by an empty one", c.Model())
	}
}
