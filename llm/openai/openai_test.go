package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
)

type mockTransport struct {
	status   int
	respBody string
	gotBody  []byte
	gotHdr   http.Header
}

func (m *mockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		m.gotBody, _ = io.ReadAll(r.Body)
	}
	m.gotHdr = r.Header
	return &http.Response{
		StatusCode: m.status,
		Body:       io.NopCloser(strings.NewReader(m.respBody)),
		Header:     make(http.Header),
	}, nil
}

func clientWith(m *mockTransport, opts ...Option) *Client {
	opts = append([]Option{WithHTTPClient(&http.Client{Transport: m})}, opts...)
	return New("test-key", opts...)
}

func TestGenerateMapsRequestAndDecodesText(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`}
	c := clientWith(m)

	resp, err := c.Generate(context.Background(), llm.Request{
		System:   "be brief",
		Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")},
		Tools:    []llm.Tool{{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopEndTurn || resp.Message.TextContent() != "hi there" {
		t.Fatalf("decode wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 4 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if m.gotHdr.Get("authorization") != "Bearer test-key" {
		t.Fatalf("auth header: %q", m.gotHdr.Get("authorization"))
	}
	var sent chatRequest
	if err := json.Unmarshal(m.gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != DefaultModel {
		t.Fatalf("model = %q", sent.Model)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" || sent.Messages[1].Role != "user" {
		t.Fatalf("messages not mapped: %+v", sent.Messages)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "echo" {
		t.Fatalf("tools not mapped: %+v", sent.Tools)
	}
}

func TestGenerateDecodesToolCall(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`}
	resp, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "go")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 || uses[0].ID != "call_1" || uses[0].Name != "echo" || string(uses[0].Input) != `{"x":1}` {
		t.Fatalf("tool call not decoded: %+v", uses)
	}
}

// TestToolResultsExpandToToolMessages pins the OpenAI-specific mapping: a tool call
// is an assistant message with tool_calls, and its result is a separate "tool"
// role message, not a block in a user turn.
func TestToolResultsExpandToToolMessages(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"usage":{}}`}
	c := clientWith(m)

	_, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{
		llm.Text(llm.RoleUser, "task"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "call_1", Name: "echo", Input: json.RawMessage(`{}`)}}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: "call_1", Content: "echoed"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var sent chatRequest
	if err := json.Unmarshal(m.gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	// Expect: user, assistant(tool_calls), tool(result).
	if len(sent.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(sent.Messages), sent.Messages)
	}
	asst := sent.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "echo" {
		t.Fatalf("assistant tool_calls wrong: %+v", asst)
	}
	tool := sent.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content == nil || *tool.Content != "echoed" {
		t.Fatalf("tool result message wrong: %+v", tool)
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   fault.Class
	}{
		{429, fault.Transient},
		{500, fault.Transient},
		{503, fault.Transient},
		{400, fault.Terminal},
		{401, fault.Terminal},
	} {
		m := &mockTransport{status: tc.status, respBody: `{"error":{"message":"boom","type":"x"}}`}
		_, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if got := fault.Classify(err); got != tc.want {
			t.Fatalf("status %d classified %s, want %s", tc.status, got, tc.want)
		}
	}
}

// TestAssistantMappingProperty pins that an assistant turn (text plus tool calls)
// survives encoding into a Chat Completions message and decoding back, which is the
// fidelity the conversatflynn replay depends on.
func TestAssistantMappingProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.StringMatching(`[a-z ]{0,10}`).Draw(rt, "text")
		nCalls := rapid.IntRange(0, 3).Draw(rt, "calls")
		blocks := make([]llm.Block, 0, 1+nCalls)
		if text != "" {
			blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: text})
		}
		for range nCalls {
			blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
				ID: rapid.StringMatching(`call_[a-z0-9]{1,5}`).Draw(rt, "id"), Name: "echo", Input: json.RawMessage(`{"x":1}`),
			}})
		}
		msg := llm.Message{Role: llm.RoleAssistant, Blocks: blocks}

		enc := encodeMessage(msg)
		if len(enc) != 1 {
			rt.Fatalf("assistant message should encode to 1 chat message, got %d", len(enc))
		}
		// Round-trip through the response decoder.
		var cr chatResponse
		cr.Choices = append(cr.Choices, struct {
			Message struct {
				Content   string         `json:"content"`
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{})
		if enc[0].Content != nil {
			cr.Choices[0].Message.Content = *enc[0].Content
		}
		cr.Choices[0].Message.ToolCalls = enc[0].ToolCalls

		dec, err := decodeResponse(cr)
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		if dec.Message.TextContent() != text {
			rt.Fatalf("text %q -> %q", text, dec.Message.TextContent())
		}
		gotUses := dec.Message.ToolUses()
		if len(gotUses) != nCalls {
			rt.Fatalf("tool calls %d -> %d", nCalls, len(gotUses))
		}
		for i, u := range gotUses {
			if u.Name != "echo" || string(u.Input) != `{"x":1}` || u.ID != msg.ToolUses()[i].ID {
				rt.Fatalf("tool call %d not preserved: %+v", i, u)
			}
		}
	})
}
