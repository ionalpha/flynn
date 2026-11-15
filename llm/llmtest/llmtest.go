// Package llmtest provides a scripted in-memory llm.Model, so the conversation loop and
// the agent runtime can be tested end to end without a provider or network.
package llmtest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// ScriptedModel returns the next turn from a fixed script on each Generate call, ignoring
// the request, and records what it was handed so a test can assert the history the loop
// fed back. Running past the script is a terminal error, which exposes a loop that took
// more turns than scripted.
type ScriptedModel struct {
	mu       sync.Mutex
	turns    []llm.Response
	calls    int
	requests []llm.Request
}

var _ llm.Model = (*ScriptedModel)(nil)

// NewScripted builds a ScriptedModel that returns the given turns in order.
func NewScripted(turns ...llm.Response) *ScriptedModel {
	return &ScriptedModel{turns: turns}
}

// ErrScriptExhausted is returned when Generate is called more times than the script has
// turns.
var ErrScriptExhausted = fault.New(fault.Terminal, "llmtest_script_exhausted", "scripted model: no turn left for this call")

// Generate implements llm.Model.
func (m *ScriptedModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.calls >= len(m.turns) {
		return llm.Response{}, ErrScriptExhausted
	}
	resp := m.turns[m.calls]
	m.calls++
	return resp, nil
}

// Calls reports how many times Generate has been invoked.
func (m *ScriptedModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Requests returns a copy of the requests Generate received, in order.
func (m *ScriptedModel) Requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]llm.Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// SayText returns a final assistant turn carrying text ([llm.StopEndTurn]).
func SayText(text string) llm.Response {
	return llm.Response{
		Message:    llm.Text(llm.RoleAssistant, text),
		StopReason: llm.StopEndTurn,
	}
}

// CallTool returns an assistant turn requesting one tool call ([llm.StopToolUse]).
func CallTool(id, name string, input json.RawMessage) llm.Response {
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: input}},
		}},
		StopReason: llm.StopToolUse,
	}
}
