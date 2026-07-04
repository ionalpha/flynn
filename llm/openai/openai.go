// Package openai adapts OpenAI's Chat Completions API to the provider-agnostic
// llm.Model port. It speaks the HTTP API directly (no vendor SDK), so the agent
// keeps its single-binary shape and the adapter stays a thin, fully-testable
// mapping. Chat Completions is stateless - the full conversation is sent on every
// call - which matches the port, and it is the format every OpenAI-compatible
// endpoint (local models, gateways) speaks, so the same adapter reaches all of
// them by changing the base URL. The default model is GPT-5.5.
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

	// grammarCache memoizes the compiled tool-call grammar. The tool set is
	// byte-identical turn to turn across a conversation, but the grammar is a pure
	// function of the tool names and schemas, so recompiling and re-rendering it on
	// every Generate is wasted work (gbnf compile + GBNF text render). A single-entry
	// cache keyed by a hash of the tools reuses the last result whenever the tool set
	// is unchanged, and simply recomputes on the rare turn it differs, so it can never
	// return a stale grammar for a different tool set. Guarded by a mutex because
	// Generate runs concurrently under fan-out.
	grammarMu    sync.Mutex
	grammarKey   uint64
	grammarValid bool // a result (below) is cached for grammarKey
	grammarStr   string
	grammarOK    bool // the cached compile succeeded
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

// WithToolGrammar makes the client constrain a tool-using request to a grammar
// compiled from the offered tools, so the backend can only sample a structurally
// valid tool call: a real tool name bound to arguments that satisfy that tool's
// schema. It targets a local runtime that honors the grammar request field (a local
// model server), which is where a weaker model needs the structural guarantee most;
// a hosted endpoint that does not recognize the field simply ignores it. The
// constraint is attached only when every offered tool's schema can be compiled, so
// a request never advertises a tool the grammar would forbid. Off by default.
func WithToolGrammar() Option {
	return func(c *Client) { c.toolGrammar = true }
}

// WithVision marks the served model as able to accept image input, so a user
// message that carries an image is encoded as OpenAI vision content (a content
// array with image_url parts) instead of being refused. It is off by default:
// an arbitrary OpenAI-compatible endpoint may serve a text-only model, and
// silently dropping an image would let a picture vanish from a turn. Enable it
// only when the configured model can see; the hosted GPT and Gemini models do.
func WithVision() Option {
	return func(c *Client) { c.vision = true }
}

// New builds a Client authenticating with apiKey. The key is held as a
// secret.Text, so it cannot leak through logging or formatting of the Client.
// With no HTTP client injected the shared core picks the governed default
// (netguard for a hosted endpoint, plain for loopback).
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

// refuseImages returns a terminal fault when any message carries an image block,
// used when the configured model cannot accept vision input. Refusing keeps an
// image from silently disappearing from a turn: the caller learns the model cannot
// see it, rather than a picture-free request going out that the user believes
// carried the image.
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
	// Grammar constrains decoding to a formal grammar so only permitted tokens are
	// sampled. A local model server applies it as a decode-time mask; an endpoint
	// that does not recognize the field ignores it, so it is safe to send anywhere.
	// It is set only when tool-call constraining is enabled (see WithToolGrammar).
	Grammar string `json:"grammar,omitempty"`
	// Temperature, TopP, and Seed pin decoding for a reproducible run. They are sent only
	// when the request asks for pinned sampling, so a free-running request is unchanged. All
	// three are standard fields a hosted or local OpenAI-compatible server understands.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is either a *string (the plain form every message uses) or a
	// []contentPart (the multimodal form a user message takes when it carries an
	// image). It is typed as any so one field serves both: a *string marshals to a
	// JSON string exactly as before, and a []contentPart marshals to the content
	// array the vision API needs. A nil interface is omitted.
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// contentPart is one element of OpenAI's multimodal content array: either a run of
// text or an image reference. A user message that carries an image is encoded as an
// array of these instead of a plain string, which is the shape the vision API needs.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

// imageURL carries an image as a data URI (data:<media-type>;base64,<bytes>), which
// inlines the image so the stateless request stays self-contained.
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

// buildRequest encodes a neutral request into the Chat Completions body and reports
// whether tool-call grammar constraining is active for it. When it is, the tool list
// is carried as a decode-time grammar instead of the tools field: a local server
// rejects a request that sets both a custom grammar and tools, and a grammar already
// names every callable tool, so the schemas are not sent twice. The caller uses the
// returned flag to decode a grammar-constrained reply, where the single tool call
// arrives as the message content rather than as a structured tool_calls entry.
func (c *Client) buildRequest(req llm.Request) (chatRequest, bool) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.maxTokens
	}
	out := chatRequest{Model: c.model, MaxCompletionTokens: maxTokens, PromptCacheKey: req.Cache.Key}
	if req.Sampling != nil {
		// Pin decoding for a reproducible run. Temperature and seed are always sent (zero is
		// greedy, a valid and maximally reproducible choice); top-p is sent only when set, so
		// the degenerate value zero never reaches the server.
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
		// A tool whose schema cannot be compiled leaves the request unconstrained and
		// falls back to advertising the tools below, so the model is never blocked from
		// calling an offered tool.
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

// toolGrammarCached returns the compiled tool-call grammar for tools, reusing the
// last result when the tool set is unchanged. It reports whether a grammar is active
// (compilation succeeded); a compile failure is cached too, so a tool set that cannot
// be constrained is not recompiled every turn only to fail again. The key is a 64-bit
// hash of the tool names and schemas; a hash collision across two genuinely different
// tool sets is the only way to return a wrong grammar, and at 64 bits that is
// negligible against the per-turn recompile it removes.
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

// toolsKey hashes the tool set into the memoization key: each tool's name and raw
// argument schema, order-independent so a reordered but identical tool set hits the
// cache. The grammar itself compiles tools in sorted order (see gbnf.buildToolAlts),
// so two orderings of the same tools yield the same grammar and must share a key.
func toolsKey(tools []llm.Tool) uint64 {
	// XOR each tool's own hash so the combination is independent of iteration order.
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
	default: // user (and system, handled separately): text and images become one
		// user message, tool results become individual tool messages.
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
					parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{
						URL: "data:" + b.Image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(b.Image.Data),
					}})
				}
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
		// A user turn with no text or image (only tool results) prepends no user
		// message. With an image present the content must be the array form; without
		// one it collapses to a plain string, the shape every endpoint accepts and
		// the callers that never send images keep sending.
		if len(parts) > 0 {
			user := chatMessage{Role: "user"}
			if hasImage {
				user.Content = parts
			} else {
				var b strings.Builder
				for _, p := range parts {
					b.WriteString(p.Text)
				}
				text := b.String()
				user.Content = &text
			}
			out = append([]chatMessage{user}, out...)
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

func decodeResponse(cr chatResponse, grammarTools map[string]bool) (llm.Response, error) {
	if len(cr.Choices) == 0 {
		return llm.Response{}, fault.New(fault.Terminal, "openai_no_choice", "openai: response had no choices")
	}
	choice := cr.Choices[0]

	// Under a tool-call grammar the tool list is not advertised, so the server returns
	// the single grammar-constrained call as message content rather than a structured
	// tool_calls entry. A reply that begins with "{" is a tool call by construction (the
	// grammar's other branch, a free-text answer, cannot start with "{"); decode it into
	// a tool use so the rest of the pipeline sees a uniform call regardless of provider.
	if len(grammarTools) > 0 && len(choice.Message.ToolCalls) == 0 {
		if call, ok := parseGrammarToolCall(choice.Message.Content, grammarTools); ok {
			return llm.Response{
				Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Kind: llm.KindToolUse, ToolUse: call}}},
				StopReason: llm.StopToolUse,
				Usage:      decodeUsage(cr),
			}, nil
		}
		// Not a tool call: a free-text final answer falls through to the text path below.
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

// decodeUsage maps the response's token accounting onto the neutral usage. This API
// caches stable prefixes automatically (no request-side marker) and reports
// prompt_tokens as the total input with the cached portion called out as a subset:
// InputTokens is the total and CacheReadTokens is how much of it was served from
// cache, with no separate cache-write charge. Endpoints differ on where they put the
// cached count, so take prompt_tokens_details.cached_tokens, falling back to the flat
// prompt_cache_hit_tokens some compatible providers use instead.
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

// parseGrammarToolCall decodes a grammar-constrained tool call from the model's
// message content. The tool-call grammar admits exactly an object of the form
// {"name": <tool>, "arguments": <object>}, so a content string whose first
// non-whitespace byte is "{" is parsed as that object. The call is accepted only when
// it names one of the constrained tools; anything else is reported as not a tool call
// so the caller can treat the reply as a free-text answer. A fresh call id is minted
// because a grammar-constrained reply carries none of its own.
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
