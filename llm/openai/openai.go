// Package openai adapts OpenAI's Chat Completions API to the provider-agnostic
// llm.Model port. It speaks the HTTP API directly (no vendor SDK), so the agent
// keeps its single-binary shape and the adapter stays a thin, fully-testable
// mapping. Chat Completions is stateless - the full conversation is sent on every
// call - which matches the port, and it is the format every OpenAI-compatible
// endpoint (local models, gateways) speaks, so the same adapter reaches all of
// them by changing the base URL. The default model is GPT-5.5.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

const (
	// DefaultModel is the model used when none is configured.
	DefaultModel   = "gpt-5.5"
	defaultBaseURL = "https://api.openai.com/v1"
)

// Client is an llm.Model backed by the OpenAI Chat Completions API.
type Client struct {
	apiKey    secret.Text
	model     string
	baseURL   string
	http      *http.Client
	maxTokens int
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

// WithBaseURL overrides the API base URL, so any OpenAI-compatible endpoint (a
// local server, a gateway) can be targeted. An unsafe URL (plaintext http to a
// non-loopback host, where the API key could be sniffed in transit) is rejected
// and the secure default is kept, so the override can never downgrade the
// transport. See llm.SafeBaseURL.
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

// WithMaxTokens sets the per-turn output ceiling (a request's own MaxTokens wins;
// 0 leaves it to the model's default).
func WithMaxTokens(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxTokens = n
		}
	}
}

// New builds a Client authenticating with apiKey. The key is held as a
// secret.Text, so it cannot leak through logging or formatting of the Client.
func New(apiKey secret.Text, opts ...Option) *Client {
	c := &Client{apiKey: apiKey, model: DefaultModel, baseURL: defaultBaseURL}
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
		return llm.Response{}, fault.Wrap(fault.Terminal, "openai_encode", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Terminal, "openai_request", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+c.apiKey.Expose())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Transient, "openai_http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return llm.Response{}, fault.Wrap(fault.Transient, "openai_read", err)
	}
	if resp.StatusCode/100 != 2 {
		return llm.Response{}, statusError(resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return llm.Response{}, fault.Wrap(fault.Terminal, "openai_decode", err)
	}
	return decodeResponse(cr)
}

// --- request building -------------------------------------------------------

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	// PromptCacheKey is an optional routing hint: requests carrying the same key and
	// a shared prefix are steered to the same backend, which raises the prompt-cache
	// hit rate. It is omitted when empty, so a request that opts out, or an endpoint
	// that does not recognize the field, is unaffected.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFuncCall `json:"function"`
}

type chatFuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // a JSON-encoded string
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatFuncDef `json:"function"`
}

type chatFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (c *Client) buildRequest(req llm.Request) chatRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	out := chatRequest{Model: c.model, MaxCompletionTokens: maxTokens, PromptCacheKey: req.Cache.Key}
	if req.System != "" {
		sys := req.System
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: &sys})
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, chatTool{
			Type:     "function",
			Function: chatFuncDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema},
		})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessage(m)...)
	}
	return out
}

// encodeMessage maps one neutral message to one or more Chat Completions messages.
// Unlike the block model, OpenAI carries each tool result as its own "tool" role
// message, so a user turn holding tool results expands into several messages.
func encodeMessage(m llm.Message) []chatMessage {
	switch m.Role {
	case llm.RoleAssistant:
		msg := chatMessage{Role: "assistant"}
		if text := m.TextContent(); text != "" {
			msg.Content = &text
		}
		for _, u := range m.ToolUses() {
			msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
				ID:       u.ID,
				Type:     "function",
				Function: chatFuncCall{Name: u.Name, Arguments: string(u.Input)},
			})
		}
		return []chatMessage{msg}
	default: // user (and system, handled separately): text becomes a user message,
		// tool results become individual tool messages.
		var out []chatMessage
		var text string
		for _, b := range m.Blocks {
			switch b.Kind {
			case llm.KindText:
				text += b.Text
			case llm.KindToolResult:
				if b.ToolResult != nil {
					content := b.ToolResult.Content
					out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolResult.ToolUseID, Content: &content})
				}
			default:
				// KindToolUse becomes assistant tool_calls elsewhere; KindOpaque has
				// no OpenAI mapping.
			}
		}
		if text != "" {
			out = append([]chatMessage{{Role: "user", Content: &text}}, out...)
		}
		return out
	}
}

// --- response decoding ------------------------------------------------------

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		// PromptCacheHitTokens is the cached-prefix count reported by some
		// OpenAI-compatible endpoints that do not use prompt_tokens_details (they
		// report the hit count as a flat field instead). It is the same quantity:
		// the part of the input served from cache.
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	} `json:"usage"`
}

func decodeResponse(cr chatResponse) (llm.Response, error) {
	if len(cr.Choices) == 0 {
		return llm.Response{}, fault.New(fault.Terminal, "openai_no_choice", "openai: response had no choices")
	}
	choice := cr.Choices[0]
	blocks := make([]llm.Block, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != "" {
		blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
			ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments),
		}})
	}
	// This API caches stable prefixes automatically (no request-side marker), and
	// reports prompt_tokens as the total input with the cached portion called out as
	// a subset. That matches the port directly: InputTokens is the total, and
	// CacheReadTokens is how much of it was served from cache. There is no separate
	// cache-write charge to report. Endpoints differ on where they put the cached
	// count: take prompt_tokens_details.cached_tokens, falling back to the flat
	// prompt_cache_hit_tokens some compatible providers use instead.
	cacheRead := cr.Usage.PromptTokensDetails.CachedTokens
	if cacheRead == 0 {
		cacheRead = cr.Usage.PromptCacheHitTokens
	}
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: blocks},
		StopReason: mapFinishReason(choice.FinishReason),
		Usage: llm.Usage{
			InputTokens:     cr.Usage.PromptTokens,
			OutputTokens:    cr.Usage.CompletionTokens,
			CacheReadTokens: cacheRead,
		},
	}, nil
}

func mapFinishReason(r string) llm.StopReason {
	switch r {
	case "tool_calls":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	default: // stop, content_filter, ...
		return llm.StopEndTurn
	}
}

// --- errors -----------------------------------------------------------------

// statusError maps an HTTP error to a fault-classified error: a rate-limit 429 and
// 5xx are transient so the worker retries; an exhausted-quota 429, and client
// errors, are terminal so the run fails fast instead of retrying an account that
// cannot succeed. OpenAI marks the quota case with the error type and code
// "insufficient_quota"; the message ("exceeded your current quota ... billing") is
// a fallback signal.
func statusError(code int, body []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = string(body)
	}
	quota := e.Error.Type == "insufficient_quota" || e.Error.Code == "insufficient_quota" ||
		containsAny(strings.ToLower(msg), "quota", "billing")
	return fault.New(llm.RetryClass(code, quota), "openai_status", fmt.Sprintf("openai: HTTP %d: %s", code, msg))
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
