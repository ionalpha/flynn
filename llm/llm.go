// Package llm is the agent's language-model port: the one interface every model backend
// implements, and the vendor-neutral message and tool vocabulary the agent reasons in.
//
// The types follow a tool-using chat model: a system prompt, a message list, callable
// tools, and a response of text or a batch of tool calls. A backend (an HTTP client, a
// subprocess driving an agent CLI, a local model) adapts them to its wire format, so the
// conversation loop, the runtime and tests depend only on this package and a backend can
// be swapped without touching them. Package llmtest supplies a scripted Model for tests.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Role identifies who produced a message.
type Role string

const (
	// RoleSystem carries standing instructions for the whole conversation.
	RoleSystem Role = "system"
	// RoleUser is input to the model: the task, and the results of tools it called.
	RoleUser Role = "user"
	// RoleAssistant is the model's output: text, tool calls or both.
	RoleAssistant Role = "assistant"
)

// BlockKind is the type of one content block within a message.
type BlockKind string

const (
	// KindText is natural-language text.
	KindText BlockKind = "text"
	// KindToolUse is the model asking to call a tool.
	KindToolUse BlockKind = "tool_use"
	// KindToolResult is a tool's outcome, fed back to the model.
	KindToolResult BlockKind = "tool_result"
	// KindOpaque is provider content the agent must replay verbatim, such as reasoning
	// blocks a provider requires echoed back on the next turn. An adapter emits it on
	// decode and splices its Raw bytes back on encode.
	KindOpaque BlockKind = "opaque"
	// KindImage is an image in a user message. A backend without vision refuses the
	// request instead of dropping the image, so a picture never goes missing from a turn.
	KindImage BlockKind = "image"
)

// Block is one piece of a message. Kind selects which of Text, ToolUse or ToolResult is
// meaningful.
type Block struct {
	Kind       BlockKind       `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolUse    *ToolUse        `json:"toolUse,omitempty"`
	ToolResult *ToolResult     `json:"toolResult,omitempty"`
	Image      *Image          `json:"image,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"` // provider-verbatim payload for KindOpaque
}

// ToolUse is a model's request to invoke a tool. ID matches it to the [ToolResult] that
// answers it, so parallel calls in one turn stay paired.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult carries a tool's output back to the model. IsError marks a failed call, so
// the model does not mistake an error string for a result.
type ToolResult struct {
	ToolUseID string `json:"toolUseID"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
}

// Image is encoded image bytes and their IANA media type (for example "image/png"). The
// bytes travel inline, so an image survives a crash-resume in the checkpoint with the rest
// of the conversation.
type Image struct {
	MediaType string `json:"mediaType"`
	Data      []byte `json:"data"`
}

// Message is one turn: a role and its ordered content blocks.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Tool declares a capability the model may call: a name, a description the model uses to
// decide when to call it, and a JSON Schema for its arguments. Execution is the caller's.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Request is one call to a model. Provider knobs such as thinking depth are absent from
// the port, so each backend applies its own defaults.
type Request struct {
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"maxTokens,omitempty"`
	Cache     CacheHint `json:"cache,omitempty"`
	// Sampling pins decoding for a reproducible run. Nil takes the server's defaults.
	Sampling *Sampling `json:"sampling,omitempty"`
}

// CacheHint tells a backend which parts of a request are stable across turns, so a
// provider with prompt caching can reuse their processing. Only the caller knows what is
// stable, so it declares it and each adapter realizes it in its own terms. A provider
// with cache markers places one at each boundary; one with automatic prefix caching, or
// none, ignores the hint and loses nothing, because the caller keeps the prefix
// byte-identical either way. The hint never changes what the model is asked or returns.
// The zero value caches nothing.
type CacheHint struct {
	// Prefix marks the system prompt and tool schemas, the largest stable region, as one
	// cacheable boundary.
	Prefix bool `json:"prefix,omitempty"`
	// StableMessages is the number of leading messages stable across turns, normally
	// the full count because a tool loop only appends. Zero leaves history uncached. A
	// backend places at most one message boundary, after message StableMessages-1.
	StableMessages int `json:"stableMessages,omitempty"`
	// Key identifies the conversation whose prefix this request shares, for example a run
	// id. A backend that routes by cache affinity sends same-key requests to the same
	// place. It is not a secret and never changes the content. Empty opts out.
	Key string `json:"key,omitempty"`
}

// StopReason is why the model ended its turn: [StopToolUse] means run the tools and
// continue, [StopEndTurn] means the turn is final.
type StopReason string

const (
	// StopEndTurn means the model finished with a final answer.
	StopEndTurn StopReason = "end_turn"
	// StopToolUse means the model wants tools run before it continues.
	StopToolUse StopReason = "tool_use"
	// StopMaxTokens means output hit the length ceiling and was cut off.
	StopMaxTokens StopReason = "max_tokens"
)

// Usage reports the token cost of a call. Zero means unreported.
//
// The cache fields mean the same across providers; each adapter normalizes its native
// counters into them. InputTokens is all input processed, including any served from
// cache. CacheReadTokens is the part of InputTokens served from a prompt cache, so the
// hit rate is CacheReadTokens over InputTokens. CacheWriteTokens is input written to the
// cache for later reuse, counted on top of InputTokens.
type Usage struct {
	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens stays zero for providers with automatic prefix caching, which
	// report no separate write.
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// Response is one model turn: the assistant message, why it stopped, and what it cost.
type Response struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stopReason"`
	Usage      Usage      `json:"usage"`
}

// Model is the provider port and the agent's only language-model dependency.
// Implementations must be safe for concurrent use and return fault-classified errors:
// transient for rate limits and 5xx failures, terminal for malformed requests.
type Model interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// ErrContextOverflow marks a prompt longer than the context window. An adapter wraps the
// provider's error with it, so a caller uses [errors.Is] instead of parsing message text.
// It is terminal: the caller must shorten the prompt or stop.
var ErrContextOverflow = errors.New("llm: prompt exceeds the context window")

// RetryClass maps an HTTP status from a model API to a fault.Class. 5xx and 429 are
// transient. A 429 for an exhausted quota or billing problem is terminal, so a worker does
// not retry for hours against an account that cannot succeed; quotaExhausted carries the
// provider's signal for that, because the status alone cannot tell a rate limit from an
// empty wallet. Other 4xx statuses are terminal.
func RetryClass(status int, quotaExhausted bool) fault.Class {
	switch {
	case status == 429 && quotaExhausted:
		return fault.Terminal
	case status == 429 || status >= 500:
		return fault.Transient
	default:
		return fault.Terminal
	}
}

// SafeBaseURL reports whether a base URL may receive a credential. It must be https,
// unless it targets loopback, where the traffic never leaves the machine. An empty string
// means the provider default and is safe.
func SafeBaseURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// Text builds a single-block text message in the given role.
func Text(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Kind: KindText, Text: text}}}
}

// ImageBlock builds an image block from encoded bytes and their media type.
func ImageBlock(mediaType string, data []byte) Block {
	return Block{Kind: KindImage, Image: &Image{MediaType: mediaType, Data: data}}
}

// SupportedImageMediaType reports whether every vision backend accepts the media type
// (PNG, JPEG, GIF or WebP), so a caller rejects an unusable image at capture.
func SupportedImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// ToolUses returns the tool calls in this message, in order.
func (m Message) ToolUses() []ToolUse {
	var out []ToolUse
	for _, b := range m.Blocks {
		if b.Kind == KindToolUse && b.ToolUse != nil {
			out = append(out, *b.ToolUse)
		}
	}
	return out
}

// TextContent concatenates the text blocks of a message, dropping tool-call blocks.
func (m Message) TextContent() string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == KindText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
