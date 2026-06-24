// Package anthropic adapts Anthropic's Messages API to the provider-agnostic
// llm.Model port. It speaks the HTTP API directly (no vendor SDK), so the agent
// keeps its single-binary, minimal-dependency shape and the adapter stays a thin,
// fully-testable mapping between this package's neutral types and the wire format.
//
// The default model is Claude Opus 4.8 with adaptive thinking. The model's
// reasoning blocks must be replayed to the API unchanged on the next turn, so they
// are carried through the conversation as llm.KindOpaque blocks: decoded verbatim
// from a response and spliced back verbatim into the next request, without the
// conversation loop ever interpreting them.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

const (
	// DefaultModel is the model used when none is configured.
	DefaultModel = "claude-opus-4-8"
	// DefaultMaxTokens bounds output when a request does not set its own ceiling.
	DefaultMaxTokens = 16000
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
)

// Client is an llm.Model backed by the Anthropic Messages API.
type Client struct {
	apiKey    string
	model     string
	baseURL   string
	http      *http.Client
	maxTokens int
	thinking  bool
}

// Option configures a Client.
type Option func(*Client)

// WithModel sets the model id (default DefaultModel).
func WithModel(m string) Option {
	return func(c *Client) {
		if m != "" {
			c.model = m
		}
	}
}

// WithBaseURL overrides the API base URL (for a proxy or a compatible endpoint).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithHTTPClient injects the HTTP client, so tests supply a mock transport and
// production can set timeouts and proxies.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithMaxTokens sets the default output ceiling (a request's own MaxTokens wins).
func WithMaxTokens(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxTokens = n
		}
	}
}

// WithThinking enables or disables adaptive thinking (default enabled).
func WithThinking(on bool) Option {
	return func(c *Client) { c.thinking = on }
}

// New builds a Client authenticating with apiKey. With no HTTP client injected it
// uses one with a generous timeout, since a single non-streaming turn can run for
// a while.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:    apiKey,
		model:     DefaultModel,
		baseURL:   defaultBaseURL,
		maxTokens: DefaultMaxTokens,
		thinking:  true,
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 10 * time.Minute}
	}
	return c
}

var _ llm.Model = (*Client)(nil)

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Terminal, "anthropic_encode", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Terminal, "anthropic_request", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Transient, "anthropic_http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Transient, "anthropic_read", err)
	}
	if resp.StatusCode/100 != 2 {
		return llm.Response{}, statusError(resp.StatusCode, raw)
	}

	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return llm.Response{}, fault.Wrap(fault.Terminal, "anthropic_decode", err)
	}
	return decodeResponse(ar)
}

// --- request building -------------------------------------------------------

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
	Tools     []apiTool    `json:"tools,omitempty"`
	Thinking  *apiThinking `json:"thinking,omitempty"`
}

type apiThinking struct {
	Type string `json:"type"`
}

type apiMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type apiTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (c *Client) buildRequest(req llm.Request) apiRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	out := apiRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  make([]apiMessage, 0, len(req.Messages)),
	}
	if c.thinking {
		out.Thinking = &apiThinking{Type: "adaptive"}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, apiMessage{Role: string(m.Role), Content: encodeBlocks(m.Blocks)})
	}
	return out
}

// encodeBlocks maps neutral blocks to Messages-API content blocks. An opaque block
// is spliced back verbatim (it is provider content we captured earlier).
func encodeBlocks(blocks []llm.Block) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(blocks))
	for _, b := range blocks {
		var v any
		switch b.Kind {
		case llm.KindText:
			v = map[string]any{"type": "text", "text": b.Text}
		case llm.KindToolUse:
			if b.ToolUse != nil {
				v = map[string]any{"type": "tool_use", "id": b.ToolUse.ID, "name": b.ToolUse.Name, "input": b.ToolUse.Input}
			}
		case llm.KindToolResult:
			if b.ToolResult != nil {
				v = map[string]any{"type": "tool_result", "tool_use_id": b.ToolResult.ToolUseID, "content": b.ToolResult.Content, "is_error": b.ToolResult.IsError}
			}
		case llm.KindOpaque:
			if len(b.Raw) > 0 {
				out = append(out, b.Raw)
			}
			continue
		}
		if v == nil {
			continue
		}
		if enc, err := json.Marshal(v); err == nil {
			out = append(out, enc)
		}
	}
	return out
}

// --- response decoding ------------------------------------------------------

type apiResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      apiUsage          `json:"usage"`
}

type apiUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func decodeResponse(ar apiResponse) (llm.Response, error) {
	blocks := make([]llm.Block, 0, len(ar.Content))
	for _, raw := range ar.Content {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return llm.Response{}, fault.Wrap(fault.Terminal, "anthropic_block_decode", err)
		}
		switch head.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &t)
			blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: t.Text})
		case "tool_use":
			var tu struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(raw, &tu)
			blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: tu.ID, Name: tu.Name, Input: tu.Input}})
		default:
			// thinking, redacted_thinking, or any future block: preserve verbatim so
			// it can be replayed unchanged on the next turn.
			blocks = append(blocks, llm.Block{Kind: llm.KindOpaque, Raw: raw})
		}
	}
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: blocks},
		StopReason: mapStopReason(ar.StopReason),
		Usage:      llm.Usage{InputTokens: ar.Usage.InputTokens, OutputTokens: ar.Usage.OutputTokens},
	}, nil
}

func mapStopReason(r string) llm.StopReason {
	switch r {
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens":
		return llm.StopMaxTokens
	default:
		// end_turn, refusal, stop_sequence, pause_turn: the turn is over.
		return llm.StopEndTurn
	}
}

// --- errors -----------------------------------------------------------------

// statusError maps an HTTP error response to a fault-classified error: rate limits
// and server errors are transient so the worker retries; client errors are terminal.
func statusError(code int, body []byte) error {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = string(body)
	}
	class := fault.Terminal
	if code == http.StatusTooManyRequests || code == 529 || code >= 500 {
		class = fault.Transient
	}
	return fault.New(class, "anthropic_status", fmt.Sprintf("anthropic: HTTP %d: %s", code, msg))
}
