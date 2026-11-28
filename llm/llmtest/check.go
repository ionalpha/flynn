package llmtest

import (
	"fmt"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

// CheckRequest returns an error for a request a hosted provider would reject with HTTP
// 400, so a fake model fails a test where the real one would fail a user. A fake that
// accepts any request lets a caller send history no provider takes, and the bug surfaces
// only against the live API.
//
// The rules are the ones Anthropic and OpenAI share: no message is empty; every tool
// call in an assistant message is answered by a result with its ID in the user message
// directly after it; every result answers a call from the assistant message directly
// before it.
func CheckRequest(req llm.Request) error {
	for i, m := range req.Messages {
		if len(m.Blocks) == 0 {
			return invalid("message %d (%s) has no content", i, m.Role)
		}
		results := map[string]bool{}
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && b.ToolResult != nil {
				results[b.ToolResult.ToolUseID] = true
			}
		}
		if len(results) > 0 {
			if m.Role != llm.RoleUser {
				return invalid("message %d carries tool results but is %s, not user", i, m.Role)
			}
			calls := map[string]bool{}
			if i > 0 && req.Messages[i-1].Role == llm.RoleAssistant {
				for _, u := range req.Messages[i-1].ToolUses() {
					calls[u.ID] = true
				}
			}
			for id := range results {
				if !calls[id] {
					return invalid("message %d answers tool call %q, which the assistant message before it did not make", i, id)
				}
			}
		}
		uses := m.ToolUses()
		if len(uses) == 0 {
			continue
		}
		if m.Role != llm.RoleAssistant {
			return invalid("message %d makes tool calls but is %s, not assistant", i, m.Role)
		}
		answered := map[string]bool{}
		if i+1 < len(req.Messages) {
			for _, b := range req.Messages[i+1].Blocks {
				if b.Kind == llm.KindToolResult && b.ToolResult != nil {
					answered[b.ToolResult.ToolUseID] = true
				}
			}
		}
		for _, u := range uses {
			if !answered[u.ID] {
				return invalid("message %d calls tool %s (%q) with no result in the message after it", i, u.Name, u.ID)
			}
		}
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fault.New(fault.Terminal, "llmtest_invalid_request", "request a provider would reject: "+fmt.Sprintf(format, args...))
}
