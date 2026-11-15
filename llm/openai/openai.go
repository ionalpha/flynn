// Package openai adapts OpenAI's Chat Completions API to the llm.Model port over plain
// HTTP, with no vendor SDK, so the agent stays a single binary. Chat Completions is
// stateless like the port, and every OpenAI-compatible endpoint (local models, gateways)
// speaks it, so the same adapter reaches all of them through the base URL.
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/internal/httpapi"
	"github.com/ionalpha/flynn/secret"
)

const (
	// DefaultModel is the model used when none is configured.
	DefaultModel   = "gpt-5.5"
	defaultBaseURL = "https://api.openai.com/v1"
)

// Client is an llm.Model backed by the OpenAI Chat Completions API.
type Client struct {
	apiKey      secret.Text
	model       string
	baseURL     string
	http        *http.Client
	api         *httpapi.Client
	maxTokens   int
	toolGrammar bool
	vision      bool

	// The grammar fields memoize the compiled tool-call grammar under one entry keyed by
	// a hash of the tools. The tool set rarely changes within a conversation, and a
	// changed set recompiles, so the cache never returns a grammar for other tools. The
	// mutex is needed because Generate runs concurrently under fan-out.
	grammarMu    sync.Mutex
	grammarKey   uint64
	grammarValid bool // a result (below) is cached for grammarKey
	grammarStr   string
	grammarOK    bool // the cached compile succeeded
}

// Option configures a Client.
type Option func(*Client)

// WithModel sets the model id (default [DefaultModel]).
func WithModel(m string) Option {
	return func(c *Client) {
		if m != "" {
			c.model = m
		}
	}
}

// WithBaseURL overrides the API base URL, for any OpenAI-compatible endpoint. A URL that
// fails llm.SafeBaseURL is ignored and the default kept, so the override can never send
// the key over plaintext to a remote host.
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

// WithMaxTokens sets the per-turn output ceiling. A request's own MaxTokens wins; 0
// leaves the model default.
func WithMaxTokens(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxTokens = n
		}
	}
}

// WithToolGrammar constrains a tool-using request to a grammar compiled from the offered
// tools, so the backend can only sample a real tool name with arguments that satisfy its
// schema. It is for local runtimes that honor the grammar field, where weaker models need
// it most; a hosted endpoint ignores the field. The grammar is attached only when every
// tool's schema compiles, so a request never offers a tool the grammar forbids. Off by
// default.
func WithToolGrammar() Option {
	return func(c *Client) { c.toolGrammar = true }
}

// WithVision marks the model as accepting images, so an image is sent as image_url
// content instead of refused. It is off by default because an OpenAI-compatible endpoint
// may serve a text-only model. The hosted GPT and Gemini models accept images.
func WithVision() Option {
	return func(c *Client) { c.vision = true }
}

// New builds a Client authenticating with apiKey. The key is a secret.Text, so it cannot
// leak through logging the Client. With no HTTP client injected, the shared core picks
// netguard for a hosted endpoint and a plain client for loopback.
func New(apiKey secret.Text, opts ...Option) *Client {
	c := &Client{apiKey: apiKey, model: DefaultModel, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	c.api = httpapi.New("openai", c.baseURL, func(h http.Header) {
		h.Set("authorization", "Bearer "+c.apiKey.Expose())
	}, c.http)
	return c
}

var _ llm.Model = (*Client)(nil)

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	if !c.vision {
		if err := refuseImages(req, c.model); err != nil {
			return llm.Response{}, err
		}
	}
	chatReq, grammarActive := c.buildRequest(req)
	var cr chatResponse
	if err := c.api.PostJSON(ctx, "/chat/completions", chatReq, &cr); err != nil {
		return llm.Response{}, err
	}
	var grammarTools map[string]bool
	if grammarActive {
		grammarTools = make(map[string]bool, len(req.Tools))
		for _, t := range req.Tools {
			grammarTools[t.Name] = true
		}
	}
	return decodeResponse(cr, grammarTools)
}

// refuseImages returns a terminal fault when a message carries an image the model cannot
// accept, so the caller learns that instead of the image disappearing from the turn.
func refuseImages(req llm.Request, model string) error {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindImage {
				return fault.New(fault.Terminal, "openai_no_vision", "openai: model "+model+" cannot accept image input")
			}
		}
	}
	return nil
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	// PromptCacheKey steers requests with the same key and prefix to the same backend,
	// raising the cache hit rate. Omitted when empty.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// Grammar is a decode-time token mask for a local server; other endpoints ignore it.
	// It is set only under [WithToolGrammar].
	Grammar string `json:"grammar,omitempty"`
	// Temperature, TopP and Seed are sent only when the request pins sampling.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is a *string for a plain message or a []contentPart for a user message
	// with an image. A nil interface is omitted.
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// contentPart is one element of OpenAI's multimodal content array: text or an image.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL carries an image as a data URI, so the stateless request stays
// self-contained.
type imageURL struct {
	URL string `json:"url"`
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

// buildRequest encodes a neutral request and reports whether a tool-call grammar is
// active. When it is, the grammar replaces the tools field, because a local server
// rejects a request with both and the grammar already names every tool. The flag tells
// the decoder the call arrives as message content instead of tool_calls.
func (c *Client) buildRequest(req llm.Request) (chatRequest, bool) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	out := chatRequest{Model: c.model, MaxCompletionTokens: maxTokens, PromptCacheKey: req.Cache.Key}
	if req.Sampling != nil {
		// Temperature and seed are always sent, since zero is greedy and valid. Top-p
		// is sent only when set, so zero never reaches the server.
		s := req.Sampling.Normalized()
		out.Temperature = &s.Temperature
		out.Seed = &s.Seed
		if s.TopP > 0 {
			out.TopP = &s.TopP
		}
	}
	if req.System != "" {
		sys := req.System
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: &sys})
	}
	grammarActive := false
	if c.toolGrammar && len(req.Tools) > 0 {
		if g, ok := c.toolGrammarCached(req.Tools); ok {
			out.Grammar = g
			grammarActive = true
		}
		// An uncompilable schema leaves the request unconstrained, so the tools are
		// advertised below instead.
	}
	if !grammarActive {
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, chatTool{
				Type:     "function",
				Function: chatFuncDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema},
			})
		}
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessage(m)...)
	}
	return out, grammarActive
}

// toolGrammarCached returns the compiled grammar for tools and whether one is active,
// reusing the last result for an unchanged tool set. A compile failure is cached too, so
// it is not retried every turn. Only a 64-bit hash collision between different tool sets
// could return a wrong grammar.
func (c *Client) toolGrammarCached(tools []llm.Tool) (string, bool) {
	key := toolsKey(tools)
	c.grammarMu.Lock()
	defer c.grammarMu.Unlock()
	if c.grammarValid && c.grammarKey == key {
		return c.grammarStr, c.grammarOK
	}
	g, err := toolCallGrammar(tools)
	c.grammarKey = key
	c.grammarValid = true
	c.grammarStr = g
	c.grammarOK = err == nil
	if err != nil {
		c.grammarStr = ""
	}
	return c.grammarStr, c.grammarOK
}

// toolsKey hashes each tool's name and schema, independent of order, because the grammar
// compiles tools in sorted order and two orderings must share a key.
func toolsKey(tools []llm.Tool) uint64 {
	// XOR makes the combination independent of iteration order.
	var key uint64
	for _, t := range tools {
		h := fnv.New64a()
		_, _ = h.Write([]byte(t.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(t.InputSchema)
		key ^= h.Sum64()
	}
	return key
}

// encodeMessage maps one neutral message to one or more Chat Completions messages,
// because OpenAI sends each tool result as its own "tool" role message.
func encodeMessage(m llm.Message) []chatMessage {
	if m.Role == llm.RoleAssistant {
		return []chatMessage{encodeAssistant(m)}
	}
	// User; system is handled separately.
	return encodeUser(m)
}

func encodeAssistant(m llm.Message) chatMessage {
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
	return msg
}

// encodeUser turns text and images into one user message and each tool result into a
// tool message. The user message goes after the tool messages, because Chat Completions
// requires every tool_call to be answered by tool messages directly after the assistant
// turn, and a user message between them is a 400. A turn carries both when something
// rides along with the results, such as a stall nudge.
func encodeUser(m llm.Message) []chatMessage {
	var out []chatMessage
	var parts []contentPart
	hasImage := false
	for _, b := range m.Blocks {
		switch b.Kind {
		case llm.KindText:
			if b.Text != "" {
				parts = append(parts, contentPart{Type: "text", Text: b.Text})
			}
		case llm.KindImage:
			if b.Image != nil {
				hasImage = true
				parts = append(parts, imagePart(b.Image))
			}
		case llm.KindToolResult:
			if b.ToolResult != nil {
				content := b.ToolResult.Content
				out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolResult.ToolUseID, Content: &content})
			}
		default:
			// KindToolUse is encoded on the assistant turn; KindOpaque has no OpenAI
			// mapping.
		}
	}
	if len(parts) == 0 {
		return out
	}
	return append(out, chatMessage{Role: "user", Content: userContent(parts, hasImage)})
}

func imagePart(img *llm.Image) contentPart {
	return contentPart{Type: "image_url", ImageURL: &imageURL{
		URL: "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
	}}
}

// userContent is the array form when an image is present, and otherwise a plain string,
// which every endpoint accepts.
func userContent(parts []contentPart, hasImage bool) any {
	if hasImage {
		return parts
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	text := b.String()
	return &text
}

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
		// PromptCacheHitTokens is the flat cached-prefix count some compatible endpoints
		// report in place of prompt_tokens_details.
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	} `json:"usage"`
}

func decodeResponse(cr chatResponse, grammarTools map[string]bool) (llm.Response, error) {
	if len(cr.Choices) == 0 {
		return llm.Response{}, fault.New(fault.Terminal, "openai_no_choice", "openai: response had no choices")
	}
	choice := cr.Choices[0]

	// Under a tool-call grammar the call arrives as message content. A reply starting
	// with "{" is a tool call, because the grammar's free-text branch cannot start
	// with "{".
	if len(grammarTools) > 0 && len(choice.Message.ToolCalls) == 0 {
		if call, ok := parseGrammarToolCall(choice.Message.Content, grammarTools); ok {
			return llm.Response{
				Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Kind: llm.KindToolUse, ToolUse: call}}},
				StopReason: llm.StopToolUse,
				Usage:      decodeUsage(cr),
			}, nil
		}
		// Not a tool call: a free-text answer falls through to the text path.
	}

	blocks := make([]llm.Block, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != "" {
		blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
			ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments),
		}})
	}
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: blocks},
		StopReason: mapFinishReason(choice.FinishReason),
		Usage:      decodeUsage(cr),
	}, nil
}

// decodeUsage maps token accounting onto llm.Usage. This API caches prefixes
// automatically and reports prompt_tokens as total input with the cached part as a
// subset, and has no cache-write charge. The cached count comes from
// prompt_tokens_details.cached_tokens, or the flat prompt_cache_hit_tokens some
// compatible providers use.
func decodeUsage(cr chatResponse) llm.Usage {
	cacheRead := cr.Usage.PromptTokensDetails.CachedTokens
	if cacheRead == 0 {
		cacheRead = cr.Usage.PromptCacheHitTokens
	}
	return llm.Usage{
		InputTokens:     cr.Usage.PromptTokens,
		OutputTokens:    cr.Usage.CompletionTokens,
		CacheReadTokens: cacheRead,
	}
}

// parseGrammarToolCall decodes a grammar-constrained call, an object of the form
// {"name": <tool>, "arguments": <object>}. It is accepted only when it names a
// constrained tool, so anything else is treated as free text. A fresh call id is minted
// because the reply carries none.
func parseGrammarToolCall(content string, grammarTools map[string]bool) (*llm.ToolUse, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(trimmed), &call); err != nil {
		return nil, false
	}
	if !grammarTools[call.Name] {
		return nil, false
	}
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return &llm.ToolUse{ID: ids.New(), Name: call.Name, Input: args}, true
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
