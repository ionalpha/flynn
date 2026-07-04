package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

// transcript builds a conversation of depth turns: each turn is an assistant
// message (a line of text plus a tool call) followed by a user message carrying the
// tool result. It is the growing history the stateless API re-encodes every turn.
func transcript(depth int) []llm.Message {
	msgs := make([]llm.Message, 0, depth*2)
	for i := range depth {
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindText, Text: "let me call a tool to make progress on step"},
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "call", Name: "read_file", Input: json.RawMessage(`{"path":"/etc/hosts"}`)}},
		}})
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: "call", Content: "127.0.0.1 localhost\n::1 localhost\n"}},
		}})
		_ = i
	}
	return msgs
}

// BenchmarkBuildRequest encodes the whole transcript into an Anthropic request at
// growing depth, the work done on every turn. With typed blocks and a single outer
// marshal, per-turn allocations track the transcript size linearly rather than
// carrying the extra map[string]any + per-block marshal the old encoder added.
func BenchmarkBuildRequest(b *testing.B) {
	c := New(secret.New("k"))
	for _, depth := range []int{1, 10, 100, 500} {
		msgs := transcript(depth)
		req := llm.Request{System: "be concise", Messages: msgs, Cache: llm.CacheHint{Prefix: true, StableMessages: len(msgs)}}
		b.Run("depth="+itoa(depth), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = c.buildRequest(req)
			}
		})
	}
}

// itoa avoids pulling strconv into the benchmark just for a label.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
