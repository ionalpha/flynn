// Package embed turns text into vectors over an OpenAI-compatible
// /v1/embeddings endpoint, which is the one embedding wire format the hosted
// providers and the local model servers all speak. Pointing it at a different
// base URL is the whole of switching backend, the same property that lets one
// chat adapter reach every OpenAI-shaped endpoint.
//
// It is the producer for memory/hybrid.Embedder. The interface lives with the
// recall that consumes it and names no provider; this package is the adapter,
// built on the same governed HTTP core as the chat adapters, so a hardening fix
// there covers embeddings too.
package embed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/internal/httpapi"
	"github.com/ionalpha/flynn/secret"
)

const (
	// DefaultModel is the embedding model used when none is configured. It is the
	// small one on purpose: recall ranking is a similarity ordering over a few
	// hundred short items, where the large model's extra dimensions buy little and
	// cost on every write.
	DefaultModel = "text-embedding-3-small"
	// DefaultBatch is how many texts go in one request. A batch bounds the request
	// body and the blast radius of one failure: a corpus embedded in batches loses a
	// batch to a timeout, where a corpus sent whole loses the read.
	DefaultBatch   = 64
	defaultBaseURL = "https://api.openai.com/v1"
)

// Client embeds text through one endpoint. Build it once and share it; it holds no
// per-call state.
type Client struct {
	apiKey  secret.Text
	model   string
	baseURL string
	http    *http.Client
	api     *httpapi.Client
	batch   int
}

// Option configures a Client.
type Option func(*Client)

// WithModel sets the embedding model id (default DefaultModel). A local server
// that serves one model ignores the field, and sending it anyway costs nothing.
func WithModel(m string) Option {
	return func(c *Client) {
		if m != "" {
			c.model = m
		}
	}
}

// WithBaseURL points the client at any OpenAI-compatible endpoint: a local model
// server, a gateway, another vendor. An unsafe URL (plaintext http to a
// non-loopback host, where the key could be read in transit) is rejected and the
// secure default kept, so an override can never downgrade the transport. See
// llm.SafeBaseURL.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" && llm.SafeBaseURL(u) {
			c.baseURL = u
		}
	}
}

// WithHTTPClient injects the HTTP client (tests supply a mock transport).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithBatch caps how many texts go in one request. A non-positive n restores the
// default.
func WithBatch(n int) Option {
	return func(c *Client) {
		if n <= 0 {
			n = DefaultBatch
		}
		c.batch = n
	}
}

// New builds a Client authenticating with apiKey, which is held as a secret.Text so
// it cannot leak through logging or formatting. An endpoint that needs no key (a
// loopback model server) takes an empty one.
func New(apiKey secret.Text, opts ...Option) *Client {
	c := &Client{apiKey: apiKey, model: DefaultModel, baseURL: defaultBaseURL, batch: DefaultBatch}
	for _, o := range opts {
		o(c)
	}
	c.api = httpapi.New("embed", c.baseURL, func(h http.Header) {
		if key := c.apiKey.Expose(); key != "" {
			h.Set("authorization", "Bearer "+key)
		}
	}, c.http)
	return c
}

// Model reports the embedding model id in use, which is what an operator checking
// what their recall is ranked by needs to be told.
func (c *Client) Model() string { return c.model }

// embedRequest is one /v1/embeddings call.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the reply. Index is read rather than assumed: the API documents
// the data array as carrying its own ordering, and a ranking built on vectors
// silently paired with the wrong text would look like a working recall that returns
// the wrong memories.
type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per text, in the same order. An empty list makes no
// call: an endpoint asked to embed nothing is a round trip that can only fail.
//
// A failure is returned rather than a short result. The caller ranking with these
// is built to fall back to a lexical order when embeddings are unavailable, and
// giving it half a corpus's vectors would fuse a ranking over a set nobody chose.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += c.batch {
		end := start + c.batch
		if end > len(texts) {
			end = len(texts)
		}
		if err := c.embedBatch(ctx, texts[start:end], out[start:end]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// embedBatch fills into with the vectors for texts, which it must do exactly: every
// slot written once, by the index the endpoint gave.
func (c *Client) embedBatch(ctx context.Context, texts []string, into [][]float32) error {
	var resp embedResponse
	if err := c.api.PostJSON(ctx, "/embeddings", embedRequest{Model: c.model, Input: texts}, &resp); err != nil {
		return err
	}
	if len(resp.Data) != len(texts) {
		return fault.New(fault.Terminal, "embed_response",
			fmt.Sprintf("embed: asked for %d vectors and got %d", len(texts), len(resp.Data)))
	}
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(into) {
			return fault.New(fault.Terminal, "embed_response",
				fmt.Sprintf("embed: vector for text %d in a batch of %d", d.Index, len(into)))
		}
		if into[d.Index] != nil {
			return fault.New(fault.Terminal, "embed_response",
				fmt.Sprintf("embed: two vectors for text %d", d.Index))
		}
		into[d.Index] = d.Embedding
	}
	return nil
}
