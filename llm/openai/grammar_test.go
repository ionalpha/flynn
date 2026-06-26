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
	raw, ok := sentBody(t, WithToolGrammar())["grammar"].(string)
	if !ok || raw == "" {
		t.Fatal("expected a non-empty grammar on the request")
	}
	// The grammar on the wire must mean what the tool requires: the well-formed
	// envelope binds the tool name to schema-valid arguments and rejects an invalid
	// call. Recompiling from the same tools gives the recognizer to check that.
	g, err := gbnf.ToolCall([]gbnf.ToolSchema{{Name: "read", Schema: grammarToolReq.Tools[0].InputSchema}})
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
