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
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/internal/httpapi"
	"github.com/ionalpha/flynn/secret"
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
	apiKey    secret.Text
	model     string
	baseURL   string
	http      *http.Client
	api       *httpapi.Client
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
// An unsafe URL (plaintext http to a non-loopback host, where the API key could be
// sniffed in transit) is rejected and the secure default is kept, so the override
// can never downgrade the transport. See llm.SafeBaseURL.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" && llm.SafeBaseURL(u) {
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

// New builds a Client authenticating with apiKey. The key is held as a
// secret.Text, so it cannot leak through logging or formatting of the Client. With
// no HTTP client injected the shared core picks the governed default (netguard for
// a hosted endpoint, plain for loopback), with a generous timeout since a single
// non-streaming turn can run for a while.
func New(apiKey secret.Text, opts ...Option) *Client {
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
	c.api = httpapi.New("anthropic", c.baseURL, func(h http.Header) {
		h.Set("x-api-key", c.apiKey.Expose())
		h.Set("anthropic-version", apiVersion)
	}, c.http)
	return c
}

var _ llm.Model = (*Client)(nil)

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	var ar apiResponse
	if err := c.api.PostJSON(ctx, "/v1/messages", c.buildRequest(req), &ar); err != nil {
		return llm.Response{}, err
	}
	return decodeResponse(ar)
}

// --- request building -------------------------------------------------------

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    any          `json:"system,omitempty"`
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
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// cacheControl marks a prefix boundary the API should cache. "ephemeral" is the
// short-lived prompt cache reused across the turns of one conversation.
type cacheControl struct {
	Type string `json:"type"`
}

func ephemeral() *cacheControl { return &cacheControl{Type: "ephemeral"} }

// systemBlock is the structured form of the system field, used only when a cache
// boundary is requested on it; otherwise the system prompt is sent as a plain
// string. The API caches the prefix up to and including the marked block, and the
// tool schemas sit before the system prompt in that prefix, so one marker here
// caches the whole static head (tools plus system) of every turn.
type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

func (c *Client) buildRequest(req llm.Request) apiRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	out := apiRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages:  make([]apiMessage, 0, len(req.Messages)),
	}
	if c.thinking {
		out.Thinking = &apiThinking{Type: "adaptive"}
	}

	// The static-prefix boundary. The cache prefix is ordered tools, then system,
	// so a marker on the system block caches both. With no system prompt, fall back
	// to marking the last tool so the tool schemas still cache.
	markLastTool := false
	switch {
	case req.Cache.Prefix && req.System != "":
		out.System = []systemBlock{{Type: "text", Text: req.System, CacheControl: ephemeral()}}
	case req.System != "":
		out.System = req.System
		markLastTool = req.Cache.Prefix
	default:
		markLastTool = req.Cache.Prefix
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, apiTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	if markLastTool && len(out.Tools) > 0 {
		out.Tools[len(out.Tools)-1].CacheControl = ephemeral()
	}

	// The rolling message boundary: cache through the last stable message so the
	// next turn reads the frozen history back instead of reprocessing it.
	markMsg := req.Cache.StableMessages
	if markMsg > len(req.Messages) {
		markMsg = len(req.Messages)
	}
	for i, m := range req.Messages {
		out.Messages = append(out.Messages, apiMessage{
			Role:    string(m.Role),
			Content: encodeBlocks(m.Blocks, markMsg > 0 && i == markMsg-1),
		})
	}
	return out
}

// encodeBlocks maps neutral blocks to Messages-API content blocks. An opaque block
// is spliced back verbatim (it is provider content we captured earlier). When
// markLast is set, the last cacheable block carries a cache_control marker, which
// makes everything up to and including it a cache boundary; opaque blocks are not
// marked, since they are replayed byte-for-byte.
func encodeBlocks(blocks []llm.Block, markLast bool) []json.RawMessage {
	markIdx := -1
	if markLast {
		for i, b := range blocks {
			if cacheableBlock(b) {
				markIdx = i
			}
		}
	}
	out := make([]json.RawMessage, 0, len(blocks))
	for i, b := range blocks {
		var v map[string]any
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
		case llm.KindImage:
			if b.Image != nil {
				v = map[string]any{"type": "image", "source": map[string]any{
					"type":       "base64",
					"media_type": b.Image.MediaType,
					"data":       base64.StdEncoding.EncodeToString(b.Image.Data),
				}}
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
		if i == markIdx {
			v["cache_control"] = ephemeral()
		}
		if enc, err := json.Marshal(v); err == nil {
			out = append(out, enc)
		}
	}
	return out
}

// cacheableBlock reports whether a block can carry a cache_control marker. Opaque
// provider content is excluded because it must be replayed unchanged.
func cacheableBlock(b llm.Block) bool {
	switch b.Kind {
	case llm.KindText:
		return true
	case llm.KindToolUse:
		return b.ToolUse != nil
	case llm.KindToolResult:
		return b.ToolResult != nil
	default:
		return false
	}
}

// --- response decoding ------------------------------------------------------

type apiResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      apiUsage          `json:"usage"`
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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
	// This API reports input_tokens as the uncached input only, with cache reads and
	// writes counted separately. The port's InputTokens is the total input processed,
	// so add them back; cache reads and writes are also surfaced on their own so a
	// caller can compute cache-hit-rate and the cheaper/dearer cache cost.
	u := ar.Usage
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: blocks},
		StopReason: mapStopReason(ar.StopReason),
		Usage: llm.Usage{
			InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
		},
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
