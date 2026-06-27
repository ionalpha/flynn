package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/gbnf"
	"github.com/ionalpha/flynn/llm"
)

var grammarToolReq = llm.Request{
	Messages: []llm.Message{llm.Text(llm.RoleUser, "read the file")},
	Tools: []llm.Tool{{
		Name:        "read",
		Description: "read a file",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}`),
	}},
}

// sentBody runs one Generate against a mock transport and returns the decoded
// request body, so a test can inspect exactly what the adapter put on the wire.
func sentBody(t *testing.T, opts ...Option) map[string]any {
	t.Helper()
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`}
	c := clientWith(m, opts...)
	if _, err := c.Generate(context.Background(), grammarToolReq); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(m.gotBody, &body); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	return body
}

func TestToolGrammarOffByDefault(t *testing.T) {
	if g, ok := sentBody(t)["grammar"]; ok {
		t.Fatalf("grammar should be absent without WithToolGrammar, got %v", g)
	}
}

func TestToolGrammarAttachedWhenEnabled(t *testing.T) {
	body := sentBody(t, WithToolGrammar())
	raw, ok := body["grammar"].(string)
	if !ok || raw == "" {
		t.Fatal("expected a non-empty grammar on the request")
	}
	// A custom grammar and a tools list cannot both be sent: a local server rejects
	// that combination, and the grammar already names every callable tool. So when the
	// grammar is attached, the tools field must be absent.
	if _, ok := body["tools"]; ok {
		t.Fatal("tools must not be sent alongside a tool-call grammar")
	}
	// The grammar on the wire must mean what the tool requires: the well-formed
	// envelope binds the tool name to schema-valid arguments and rejects an invalid
	// call, while still admitting a free-text final answer. Recompiling from the same
	// tools gives the recognizer to check that.
	g, err := gbnf.ToolCallOrText([]gbnf.ToolSchema{{Name: "read", Schema: grammarToolReq.Tools[0].InputSchema}})
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if g.String() != raw {
		t.Fatalf("grammar on the wire differs from the compiled grammar\nwire:\n%s\ncompiled:\n%s", raw, g.String())
	}
	if !g.Accepts(`{"name":"read","arguments":{"path":"a.go"}}`) {
		t.Error("grammar should accept a valid call")
	}
	if g.Accepts(`{"name":"read","arguments":{}}`) {
		t.Error("grammar should reject a call missing the required path")
	}
	if !g.Accepts("All three files are present.") {
		t.Error("grammar should accept a free-text final answer")
	}
}

// decodeRaw unmarshals a Chat Completions response body and decodes it the way
// Generate does, so a test can drive decoding from the exact JSON a server returns.
func decodeRaw(t *testing.T, raw string, grammarTools map[string]bool) llm.Response {
	t.Helper()
	var cr chatResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	dec, err := decodeResponse(cr, grammarTools)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return dec
}

func TestDecodeGrammarToolCallFromContent(t *testing.T) {
	// A grammar-constrained server returns the single tool call as message content,
	// not as a structured tool_calls entry. Decoding must recover it as a tool use.
	raw := `{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"read\",\"arguments\":{\"path\":\"a.go\"}}"},"finish_reason":"stop"}]}`
	dec := decodeRaw(t, raw, map[string]bool{"read": true})
	if dec.StopReason != llm.StopToolUse {
		t.Fatalf("a recovered tool call must stop for tool use, got %q", dec.StopReason)
	}
	uses := dec.Message.ToolUses()
	if len(uses) != 1 || uses[0].Name != "read" {
		t.Fatalf("want one read tool use, got %+v", uses)
	}
	if uses[0].ID == "" {
		t.Error("a recovered tool call must be given an id")
	}
	if string(uses[0].Input) != `{"path":"a.go"}` {
		t.Errorf("arguments not preserved: %s", uses[0].Input)
	}
}

func TestDecodeGrammarFreeTextIsFinalAnswer(t *testing.T) {
	// Under the same grammar, a reply that is not a tool call is the model's final
	// answer and must decode as ordinary end-of-turn text, not a tool use.
	raw := `{"choices":[{"message":{"role":"assistant","content":"All three files are present."},"finish_reason":"stop"}]}`
	dec := decodeRaw(t, raw, map[string]bool{"read": true})
	if dec.StopReason != llm.StopEndTurn {
		t.Fatalf("a free-text answer must end the turn, got %q", dec.StopReason)
	}
	if len(dec.Message.ToolUses()) != 0 {
		t.Fatal("a free-text answer must not become a tool use")
	}
	if dec.Message.TextContent() != "All three files are present." {
		t.Errorf("text not preserved: %q", dec.Message.TextContent())
	}
}

func TestToolGrammarSkippedForUnsupportedSchema(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`}
	c := clientWith(m, WithToolGrammar())
	req := llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "go")},
		Tools:    []llm.Tool{{Name: "weird", InputSchema: json.RawMessage(`{"type":"geo"}`)}},
	}
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(m.gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["grammar"]; ok {
		t.Fatal("a tool with an uncompilable schema should leave the request unconstrained, not partially constrained")
	}
}

func TestToolGrammarNoToolsNoGrammar(t *testing.T) {
	m := &mockTransport{status: 200, respBody: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`}
	c := clientWith(m, WithToolGrammar())
	if _, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")}}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(m.gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["grammar"]; ok {
		t.Fatal("no tools means no grammar")
	}
}
