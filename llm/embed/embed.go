// Package embed turns text into vectors over an OpenAI-compatible /v1/embeddings
// endpoint, the one embedding format hosted providers and local servers share, so the
// base URL alone picks the backend. It produces memory/hybrid.Embedder over the same
// governed HTTP core as the chat adapters, so a hardening fix there covers embeddings.
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
	// DefaultModel is the small model, because recall ranks a few hundred short items,
	// where the large model's extra dimensions buy little and cost on every write.
	DefaultModel = "text-embedding-3-small"
	// DefaultBatch is how many texts go in one request. Batching bounds the body and
	// what one timeout loses.
	DefaultBatch   = 64
	defaultBaseURL = "https://api.openai.com/v1"
)

// Client embeds text through one endpoint. It holds no per-call state, so build one and
// share it.
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

// WithModel sets the embedding model id (default [DefaultModel]). A single-model local
// server ignores it.
func WithModel(m string) Option {
	return func(c *Client) {
		if m != "" {
			c.model = m
		}
	}
}

// WithBaseURL points the client at any OpenAI-compatible endpoint. A URL that fails
// llm.SafeBaseURL is ignored and the default kept, so the override can never send the
// key over plaintext to a remote host.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" && llm.SafeBaseURL(u) {
			c.baseURL = u
		}
	}
}

// WithHTTPClient injects the HTTP client.
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

// New builds a Client authenticating with apiKey, a secret.Text so it cannot leak
// through logging. A loopback server that needs no key takes an empty one.
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

// Model reports the embedding model id, so an operator can see what recall ranks by.
func (c *Client) Model() string { return c.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the reply. Index is read, never assumed, because vectors paired with
// the wrong text give a recall that looks healthy and returns the wrong memories.
type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per text, in order. An empty list makes no call. A failure is
// returned instead of a partial result, because the caller falls back to lexical
// ranking, and half a corpus's vectors would rank an arbitrary subset.
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

// embedBatch fills into with the vectors for texts, each slot written exactly once at
// the index the endpoint gave.
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
