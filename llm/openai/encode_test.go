package openai

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// TestEncodeMessage pins the wire shape of every message the encoder maps,
// including the blocks it drops and the order of tool messages against the
// user message that rides with them.
func TestEncodeMessage(t *testing.T) {
	text := func(s string) llm.Block { return llm.Block{Kind: llm.KindText, Text: s} }
	result := func(id, content string) llm.Block {
		return llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: id, Content: content}}
	}
	use := func(id, name, input string) llm.Block {
		return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}}
	}
	cases := []struct {
		name string
		msg  llm.Message
		want string
	}{
		{
			"assistant text",
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text("hi "), text("there")}},
			`[{"role":"assistant","content":"hi there"}]`,
		},
		{
			"assistant empty text omits content",
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text("")}},
			`[{"role":"assistant"}]`,
		},
		{
			"assistant text and tool calls",
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				text("calling"), use("c1", "echo", `{"a":1}`), use("c2", "ls", `{}`),
			}},
			`[{"role":"assistant","content":"calling","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"echo","arguments":"{\"a\":1}"}},` +
				`{"id":"c2","type":"function","function":{"name":"ls","arguments":"{}"}}]}]`,
		},
		{
			"assistant ignores tool results and nil tool use",
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse}, result("x", "y"),
			}},
			`[{"role":"assistant"}]`,
		},
		{
			"user text blocks join into a string",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("a"), text("b")}},
			`[{"role":"user","content":"ab"}]`,
		},
		{
			"user empty text adds no message",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("")}},
			`null`,
		},
		{"user with no blocks", llm.Message{Role: llm.RoleUser}, `null`},
		{
			"tool results become tool messages",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{result("c1", "one"), result("c2", "")}},
			`[{"role":"tool","content":"one","tool_call_id":"c1"},{"role":"tool","content":"","tool_call_id":"c2"}]`,
		},
		{
			"user text follows the tool results",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("nudge"), result("c1", "one")}},
			`[{"role":"tool","content":"one","tool_call_id":"c1"},{"role":"user","content":"nudge"}]`,
		},
		{
			"image forces the array form",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				text("see"), llm.ImageBlock("image/png", []byte{1, 2, 3}),
			}},
			`[{"role":"user","content":[{"type":"text","text":"see"},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,AQID"}}]}]`,
		},
		{
			"image alone",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{llm.ImageBlock("image/jpeg", []byte{0})}},
			`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AA=="}}]}]`,
		},
		{
			"nil image and nil result are dropped, opaque and tool use ignored",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				{Kind: llm.KindImage}, {Kind: llm.KindToolResult}, {Kind: llm.KindOpaque, Raw: json.RawMessage(`{}`)}, use("c", "n", `{}`),
			}},
			`null`,
		},
		{
			"image with results keeps tool messages first",
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				llm.ImageBlock("image/png", []byte{1}), result("c1", "r"),
			}},
			`[{"role":"tool","content":"r","tool_call_id":"c1"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AQ=="}}]}]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(encodeMessage(c.msg))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("encodeMessage =\n %s\nwant\n %s", got, c.want)
			}
		})
	}
}
