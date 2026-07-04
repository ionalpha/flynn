package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/internal/gbnf"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
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

func TestToolGrammarMemoization(t *testing.T) {
	c := New(secret.New("k"), WithToolGrammar())
	readTool := llm.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	writeTool := llm.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object","properties":{"data":{"type":"string"}}}`)}

	// A repeat call with the same tool set is a cache hit and must return the same
	// grammar bytes it compiled the first time.
	g1, ok1 := c.toolGrammarCached([]llm.Tool{readTool})
	g1b, ok1b := c.toolGrammarCached([]llm.Tool{readTool})
	if !ok1 || !ok1b || g1 == "" || g1 != g1b {
		t.Fatalf("same tool set must hit the cache with identical grammar: %q vs %q", g1, g1b)
	}

	// A different tool set must recompile, never return the previous grammar. This is
	// the stale-cache guard: a single-entry cache that ignored the key would hand back
	// the read-only grammar for a write-only request.
	g2, ok2 := c.toolGrammarCached([]llm.Tool{writeTool})
	if !ok2 || g2 == g1 {
		t.Fatalf("changed tool set must produce a different grammar, got stale %q", g2)
	}
	want, err := toolCallGrammar([]llm.Tool{writeTool})
	if err != nil || g2 != want {
		t.Fatalf("cached grammar for the new tool set is wrong:\n got: %s\nwant: %s", g2, want)
	}

	// Order does not change the grammar (it compiles tools in sorted order), so a
	// reordered but identical set is the same grammar.
	set := []llm.Tool{readTool, writeTool}
	reordered := []llm.Tool{writeTool, readTool}
	gA, _ := c.toolGrammarCached(set)
	gB, _ := c.toolGrammarCached(reordered)
	if gA == "" || gA != gB {
		t.Fatalf("reordered identical tool set must yield the same grammar:\n%s\n%s", gA, gB)
	}

	// A tool whose schema cannot be compiled caches the failure and leaves the request
	// unconstrained; the cached miss must stay a miss on repeat.
	bad := []llm.Tool{{Name: "weird", InputSchema: json.RawMessage(`{"type":"geo"}`)}}
	if g, ok := c.toolGrammarCached(bad); ok || g != "" {
		t.Fatalf("uncompilable schema must not be constrained: ok=%v g=%q", ok, g)
	}
	if g, ok := c.toolGrammarCached(bad); ok || g != "" {
		t.Fatalf("cached compile failure must stay a miss: ok=%v g=%q", ok, g)
	}
}

// TestToolGrammarCachedAllocsCeiling locks the memoization win in the shape the perf
// gate trusts: the steady-state cache-hit path (unchanged tool set) allocates nothing,
// so a regression that reintroduces per-turn recompilation on the local-model path
// fails here rather than silently. Runs inside ordinary go test, no -bench needed.
func TestToolGrammarCachedAllocsCeiling(t *testing.T) {
	c := New(secret.New("k"), WithToolGrammar())
	tools := []llm.Tool{
		{Name: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
	}
	if _, ok := c.toolGrammarCached(tools); !ok { // prime the cache
		t.Fatal("grammar did not compile")
	}
	avg := testing.AllocsPerRun(100, func() {
		if _, ok := c.toolGrammarCached(tools); !ok {
			t.Fatal("cache miss on a primed tool set")
		}
	})
	if avg != 0 {
		t.Fatalf("cached tool-grammar lookup must not allocate, got %.0f allocs/op", avg)
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
