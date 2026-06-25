// Package llm is the agent's provider-agnostic language-model port: the single
// interface every model backend implements, and the neutral message/tool
// vocabulary the agent reasons in. It is deliberately not tied to any vendor.
//
// The shape is the proven request/response of a tool-using chat model (a system
// prompt, a running list of messages, a set of callable tools, and a response
// that is either text or a batch of tool calls), but expressed in this package's
// own types. A backend adapts those types to its wire format: a direct HTTP/SDK
// client for a hosted model, a subprocess driving an agent CLI, or a local model.
// None of that leaks here, so the conversation loop, the agent runtime, and tests
// depend only on this port and a backend is swapped without touching them.
//
// Like state, spine, jobs, and bus, this is a port with a zero-dependency default
// for tests (a deterministic scripted Model, see fake.go) and real backends as
// out-of-tree adapters held to the same contract.
package llm

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// Role identifies who produced a message.
type Role string

const (
	// RoleSystem carries standing instructions that frame the whole conversation.
	RoleSystem Role = "system"
	// RoleUser is input to the model: the task, and the results of tools it called.
	RoleUser Role = "user"
	// RoleAssistant is the model's own output: text and/or tool calls.
	RoleAssistant Role = "assistant"
)

// BlockKind is the type of one content block within a message.
type BlockKind string

const (
	// KindText is a run of natural-language text.
	KindText BlockKind = "text"
	// KindToolUse is the model asking to call a tool, with arguments.
	KindToolUse BlockKind = "tool_use"
	// KindToolResult is the outcome of a tool call, fed back to the model.
	KindToolResult BlockKind = "tool_result"
	// KindOpaque is provider-specific content the agent does not interpret but must
	// preserve and replay verbatim, such as a model's reasoning blocks that the
	// provider requires echoed back unchanged on the next turn. An adapter emits it
	// when decoding a response and splices its Raw bytes back when encoding a
	// request; the conversation loop carries it through untouched.
	KindOpaque BlockKind = "opaque"
)

// Block is one piece of a message. Exactly one of Text, ToolUse, or ToolResult is
// meaningful, selected by Kind, so a message is an ordered mix of prose and tool
// interaction rather than a single string.
type Block struct {
	Kind       BlockKind       `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolUse    *ToolUse        `json:"toolUse,omitempty"`
	ToolResult *ToolResult     `json:"toolResult,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"` // provider-verbatim payload for KindOpaque
}

// ToolUse is a model's request to invoke a tool. ID correlates this call with the
// ToolResult that answers it, so parallel calls in one turn stay matched.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult carries a tool's output back to the model. ToolUseID matches the
// ToolUse it answers; IsError marks a failed call so the model can adapt rather
// than mistake an error string for a successful result.
type ToolResult struct {
	ToolUseID string `json:"toolUseID"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
}

// Message is one turn in the conversation: a role and its ordered content blocks.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Tool describes a capability the model may call: a name, a description the model
// uses to decide when to call it, and a JSON Schema for its arguments. It is the
// declaration only; execution is the caller's (see the mission package).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Request is one call to a model: standing instructions, the conversation so far,
// the tools on offer, and a ceiling on output length. Provider-specific knobs
// (thinking depth, sampling, caching) are intentionally absent from the port; a
// backend applies its own sensible defaults, and a richer typed surface can be
// added behind the same interface if a real need appears.
type Request struct {
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"maxTokens,omitempty"`
}

// StopReason is why the model ended its turn. It drives the conversation loop:
// ToolUse means run the requested tools and continue; EndTurn means the model is
// done and the turn is final.
type StopReason string

const (
	// StopEndTurn means the model finished its turn with a final answer.
	StopEndTurn StopReason = "end_turn"
	// StopToolUse means the model wants to call one or more tools before continuing.
	StopToolUse StopReason = "tool_use"
	// StopMaxTokens means output hit the length ceiling and was cut off.
	StopMaxTokens StopReason = "max_tokens"
)

// Usage reports the token cost of a call, so a caller can meter spend and enforce
// budgets. Zero is a valid "unknown/unreported" value for backends that do not
// surface counts.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Response is one model turn: the assistant message it produced, why it stopped,
// and what it cost.
type Response struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stopReason"`
	Usage      Usage      `json:"usage"`
}

// Model is the provider port: turn a Request into one assistant Response. It is
// the entire surface a backend implements and the only language-model dependency
// the rest of the agent has. Implementations should be safe for concurrent use
// and should return fault-classified errors (transient for rate limits and
// 5xx-style failures so the caller retries; terminal for malformed requests).
type Model interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// SafeBaseURL reports whether a base URL is safe to send a credential to. A model
// request carries the API key in a header, so the transport must be encrypted: the
// URL must be https, unless it targets the loopback host (a local model server or
// gateway), where plaintext http is allowed because the traffic never leaves the
// machine. An empty string means "use the provider default" and is reported safe.
// Backends and the provider resolver use this to refuse a plaintext remote
// endpoint, so a credential is never sent where it could be sniffed in transit.
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

// --- ergonomic constructors -------------------------------------------------

// Text builds a single-block text message in the given role.
func Text(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Kind: KindText, Text: text}}}
}

// ToolUses returns the tool calls the assistant requested in this message, in
// order, so the loop can execute them without re-walking block kinds at call sites.
func (m Message) ToolUses() []ToolUse {
	var out []ToolUse
	for _, b := range m.Blocks {
		if b.Kind == KindToolUse && b.ToolUse != nil {
			out = append(out, *b.ToolUse)
		}
	}
	return out
}

// TextContent concatenates the text blocks of a message, the human-readable answer
// with any tool-call blocks dropped.
func (m Message) TextContent() string {
	var s string
	var sSb170 strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == KindText {
			sSb170.WriteString(b.Text)
		}
	}
	s += sSb170.String()
	return s
}
