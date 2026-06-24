// Package llmtest provides a deterministic in-memory llm.Model for tests: a
// scripted backend that returns a fixed sequence of turns and records the requests
// it was given, so the conversation loop and the agent runtime can be driven
// end to end without a real provider or any network.
package llmtest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// ScriptedModel is a deterministic llm.Model: each Generate call returns the next
// turn from a fixed script and records the request it was handed. It ignores the
// request when choosing what to return, so a test fixes the model's behaviour up
// front and then asserts, via Requests, that the loop fed the right history back.
// Running off the end of the script is a terminal error, surfacing a loop that
// took more turns than the test scripted.
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

// ErrScriptExhausted is returned (terminal) when Generate is called more times
// than the script has turns.
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

// Requests returns a copy of the requests Generate has received, in order, so a
// test can assert the conversation the loop built (e.g. that tool results were
// appended before the next turn).
func (m *ScriptedModel) Requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]llm.Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// --- turn constructors ------------------------------------------------------

// SayText returns a final assistant turn carrying text (StopEndTurn).
func SayText(text string) llm.Response {
	return llm.Response{
		Message:    llm.Text(llm.RoleAssistant, text),
		StopReason: llm.StopEndTurn,
	}
}

// CallTool returns an assistant turn that requests one tool call (StopToolUse).
func CallTool(id, name string, input json.RawMessage) llm.Response {
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: input}},
		}},
		StopReason: llm.StopToolUse,
	}
}
