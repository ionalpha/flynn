package anthropic

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

// mockTransport returns a canned response and captures the request it was given.
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
	m := &mockTransport{status: 200, respBody: `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`}
	c := clientWith(m)

	resp, err := c.Generate(context.Background(), llm.Request{
		System:   "be brief",
		Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")},
		Tools:    []llm.Tool{{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopEndTurn || resp.Message.TextContent() != "hello" {
		t.Fatalf("decoded response wrong: %+v", resp)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage not decoded: %+v", resp.Usage)
	}

	// Headers and request body must be well-formed.
	if m.gotHdr.Get("x-api-key") != "test-key" || m.gotHdr.Get("anthropic-version") != apiVersion {
		t.Fatalf("auth/version headers wrong: %v", m.gotHdr)
	}
	var sent apiRequest
	if err := json.Unmarshal(m.gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != DefaultModel || sent.System != "be brief" || sent.MaxTokens != DefaultMaxTokens {
		t.Fatalf("request fields wrong: %+v", sent)
	}
	if sent.Thinking == nil || sent.Thinking.Type != "adaptive" {
		t.Fatalf("adaptive thinking not requested: %+v", sent.Thinking)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Name != "echo" {
		t.Fatalf("tools not mapped: %+v", sent.Tools)
	}
}

func TestGenerateDecodesToolUse(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"content":[{"type":"tool_use","id":"t1","name":"echo","input":{"x":1}}],"stop_reason":"tool_use","usage":{}}`}
	resp, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "go")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 1 || uses[0].Name != "echo" || string(uses[0].Input) != `{"x":1}` {
		t.Fatalf("tool use not decoded: %+v", uses)
	}
}

// TestThinkingBlockRoundTrips is the adaptive-thinking contract: a reasoning block
// comes back as opaque and, when carried into the next request, is spliced back
// verbatim, so the model sees its own reasoning unchanged.
func TestThinkingBlockRoundTrips(t *testing.T) {
	thinking := `{"type":"thinking","thinking":"let me think","signature":"sig123"}`
	m := &mockTransport{status: 200, respBody: `{"content":[` + thinking + `,{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{}}`}
	c := clientWith(m)

	resp, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "q")}})
	if err != nil {
		t.Fatal(err)
	}
	var opaque *llm.Block
	for i := range resp.Message.Blocks {
		if resp.Message.Blocks[i].Kind == llm.KindOpaque {
			opaque = &resp.Message.Blocks[i]
		}
	}
	if opaque == nil || string(opaque.Raw) != thinking {
		t.Fatalf("thinking not captured as opaque: %+v", resp.Message.Blocks)
	}

	// Send it back: the request must carry the thinking block byte-for-byte.
	if _, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{resp.Message}}); err != nil {
		t.Fatal(err)
	}
	var sent apiRequest
	if err := json.Unmarshal(m.gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, blk := range sent.Messages[0].Content {
		if string(blk) == thinking {
			found = true
		}
	}
	if !found {
		t.Fatalf("opaque thinking block not replayed verbatim: %s", m.gotBody)
	}
}

func TestThinkingCanBeDisabled(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"content":[],"stop_reason":"end_turn","usage":{}}`}
	if _, err := clientWith(m, WithThinking(false)).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}}); err != nil {
		t.Fatal(err)
	}
	var sent apiRequest
	_ = json.Unmarshal(m.gotBody, &sent)
	if sent.Thinking != nil {
		t.Fatalf("thinking should be omitted when disabled: %+v", sent.Thinking)
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   fault.Class
	}{
		{429, fault.Transient},
		{529, fault.Transient},
		{500, fault.Transient},
		{400, fault.Terminal},
		{401, fault.Terminal},
	} {
		m := &mockTransport{status: tc.status, respBody: `{"type":"error","error":{"type":"x","message":"boom"}}`}
		_, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if got := fault.Classify(err); got != tc.want {
			t.Fatalf("status %d classified %s, want %s", tc.status, got, tc.want)
		}
	}
}

// TestBlockMappingProperty pins that assistant content (text, tool calls, and
// opaque provider blocks) survives the encode-into-request then decode-from-response
// mapping unchanged. This is the fidelity the thinking-block replay depends on.
func TestBlockMappingProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 5).Draw(rt, "n")
		blocks := make([]llm.Block, 0, n)
		for i := 0; i < n; i++ {
			switch rapid.IntRange(0, 2).Draw(rt, "kind") {
			case 0:
				blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: rapid.StringMatching(`[a-z ]{0,10}`).Draw(rt, "text")})
			case 1:
				blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
					ID: rapid.StringMatching(`[a-z0-9]{1,6}`).Draw(rt, "id"), Name: "echo", Input: json.RawMessage(`{"x":1}`),
				}})
			default:
				blocks = append(blocks, llm.Block{Kind: llm.KindOpaque, Raw: json.RawMessage(`{"type":"thinking","thinking":"` + rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, "th") + `"}`)})
			}
		}

		// Encode as request content, then decode as if it were a response.
		decoded, err := decodeResponse(apiResponse{Content: encodeBlocks(blocks)})
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		got := decoded.Message.Blocks
		if len(got) != len(blocks) {
			rt.Fatalf("block count %d -> %d", len(blocks), len(got))
		}
		for i := range blocks {
			if got[i].Kind != blocks[i].Kind {
				rt.Fatalf("block %d kind %s -> %s", i, blocks[i].Kind, got[i].Kind)
			}
			switch blocks[i].Kind {
			case llm.KindText:
				if got[i].Text != blocks[i].Text {
					rt.Fatalf("text %q -> %q", blocks[i].Text, got[i].Text)
				}
			case llm.KindToolUse:
				if got[i].ToolUse.Name != blocks[i].ToolUse.Name || string(got[i].ToolUse.Input) != string(blocks[i].ToolUse.Input) {
					rt.Fatalf("tool use not preserved: %+v -> %+v", blocks[i].ToolUse, got[i].ToolUse)
				}
			case llm.KindOpaque:
				if string(got[i].Raw) != string(blocks[i].Raw) {
					rt.Fatalf("opaque %s -> %s", blocks[i].Raw, got[i].Raw)
				}
			}
		}
	})
}
