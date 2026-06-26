package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
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
	return New(secret.New("test-key"), opts...)
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
		name   string
		status int
		body   string
		want   fault.Class
	}{
		{"rate-limit 429 retries", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`, fault.Transient},
		// A billing/credit problem can arrive as a 429; it is permanent, so it must
		// be terminal and fail fast rather than retry.
		{"credit 429 is terminal", 429, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the API."}}`, fault.Terminal},
		{"529 overloaded retries", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, fault.Transient},
		{"500 retries", 500, `{"type":"error","error":{"type":"api_error","message":"internal"}}`, fault.Transient},
		{"400 bad request is terminal", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`, fault.Terminal},
		{"400 credit balance is terminal", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`, fault.Terminal},
		{"401 auth is terminal", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, fault.Terminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockTransport{status: tc.status, respBody: tc.body}
			_, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
			if err == nil {
				t.Fatalf("status %d: expected error", tc.status)
			}
			if got := fault.Classify(err); got != tc.want {
				t.Fatalf("status %d classified %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}

// TestBlockMappingProperty pins that assistant content (text, tool calls, and
// opaque provider blocks) survives the encode-into-request then decode-from-response
// mapping unchanged. This is the fidelity the thinking-block replay depends on.
func TestBlockMappingProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 5).Draw(rt, "n")
		blocks := make([]llm.Block, 0, n)
		for range n {
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
		decoded, err := decodeResponse(apiResponse{Content: encodeBlocks(blocks, false)})
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

// --- prompt caching ---------------------------------------------------------

// asMap marshals a built request and reads it back as a generic tree, so a test
// can assert where cache_control markers landed without depending on the typed
// request shape.
func asMap(t *testing.T, req apiRequest) map[string]any {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// blockHasCache reports whether a content block (a generic map) carries an
// ephemeral cache_control marker.
func blockHasCache(block any) bool {
	m, ok := block.(map[string]any)
	if !ok {
		return false
	}
	cc, ok := m["cache_control"].(map[string]any)
	return ok && cc["type"] == "ephemeral"
}

func lastContentBlock(t *testing.T, msg any) any {
	t.Helper()
	content, ok := msg.(map[string]any)["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message has no content blocks: %v", msg)
	}
	return content[len(content)-1]
}

func TestCacheHintMarksSystemAndRollingMessage(t *testing.T) {
	c := New(secret.New("k"))
	req := llm.Request{
		System: "be brief",
		Tools:  []llm.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []llm.Message{
			llm.Text(llm.RoleUser, "first"),
			llm.Text(llm.RoleAssistant, "answer"),
		},
		Cache: llm.CacheHint{Prefix: true, StableMessages: 2},
	}
	got := asMap(t, c.buildRequest(req))

	// With a system prompt present, the static-prefix marker rides on the system
	// block (which caches the tools sitting before it too), not on a tool.
	sys, ok := got["system"].([]any)
	if !ok || len(sys) != 1 || !blockHasCache(sys[0]) {
		t.Fatalf("system block not marked for caching: %v", got["system"])
	}
	tools := got["tools"].([]any)
	if blockHasCache(tools[len(tools)-1]) {
		t.Fatalf("tool should not be marked when system carries the prefix marker: %v", tools)
	}

	// The rolling boundary marks the last block of message StableMessages-1, and
	// nothing earlier.
	msgs := got["messages"].([]any)
	if blockHasCache(lastContentBlock(t, msgs[0])) {
		t.Fatalf("message 0 should not be a cache boundary: %v", msgs[0])
	}
	if !blockHasCache(lastContentBlock(t, msgs[1])) {
		t.Fatalf("message 1 (StableMessages-1) should be a cache boundary: %v", msgs[1])
	}
}

func TestCacheHintFallsBackToToolWhenNoSystem(t *testing.T) {
	c := New(secret.New("k"))
	req := llm.Request{
		Tools:    []llm.Tool{{Name: "a", InputSchema: json.RawMessage(`{}`)}, {Name: "b", InputSchema: json.RawMessage(`{}`)}},
		Messages: []llm.Message{llm.Text(llm.RoleUser, "x")},
		Cache:    llm.CacheHint{Prefix: true},
	}
	got := asMap(t, c.buildRequest(req))
	if _, present := got["system"]; present {
		t.Fatalf("system should be omitted when empty: %v", got["system"])
	}
	tools := got["tools"].([]any)
	if blockHasCache(tools[0]) {
		t.Fatalf("only the last tool should carry the prefix marker: %v", tools[0])
	}
	if !blockHasCache(tools[1]) {
		t.Fatalf("last tool should carry the prefix marker when there is no system: %v", tools[1])
	}
}

func TestNoCacheHintLeavesRequestUnmarked(t *testing.T) {
	c := New(secret.New("k"))
	req := llm.Request{
		System:   "sys",
		Tools:    []llm.Tool{{Name: "a", InputSchema: json.RawMessage(`{}`)}},
		Messages: []llm.Message{llm.Text(llm.RoleUser, "x")},
	}
	got := asMap(t, c.buildRequest(req))
	// Without a hint the system stays a plain string and nothing is marked.
	if _, isString := got["system"].(string); !isString {
		t.Fatalf("system should be a plain string without a cache hint: %T", got["system"])
	}
	if blockHasCache(got["tools"].([]any)[0]) {
		t.Fatal("tool marked without a cache hint")
	}
	if blockHasCache(lastContentBlock(t, got["messages"].([]any)[0])) {
		t.Fatal("message marked without a cache hint")
	}
}

func TestCacheUsageNormalized(t *testing.T) {
	// This API reports uncached input only, with cache reads and writes separate.
	// The port's InputTokens must be the reconstructed total.
	m := &mockTransport{status: 200, respBody: `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":20,"cache_read_input_tokens":70}}`}
	resp, err := clientWith(m).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 100 {
		t.Fatalf("InputTokens should be total processed (10+20+70=100), got %d", resp.Usage.InputTokens)
	}
	if resp.Usage.CacheReadTokens != 70 || resp.Usage.CacheWriteTokens != 20 {
		t.Fatalf("cache tokens not normalized: %+v", resp.Usage)
	}
}

// stripCacheControl removes every cache_control key from a decoded JSON tree and
// returns how many it removed, so a test can compare a marked build against an
// unmarked one and separately bound the marker count.
func stripCacheControl(v any) int {
	switch t := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := t["cache_control"]; ok {
			delete(t, "cache_control")
			n++
		}
		for _, child := range t {
			n += stripCacheControl(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range t {
			n += stripCacheControl(child)
		}
		return n
	default:
		return 0
	}
}

func tree(t *rapid.T, req apiRequest) map[string]any {
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// normalizeSystem collapses the structured single-text-block system form back to a
// plain string. Attaching a cache marker to the system requires the block form, and
// the API treats a string and a one-text-block array as the same prompt, so the
// additivity check normalizes that one sanctioned shape change before comparing.
func normalizeSystem(m map[string]any) {
	arr, ok := m["system"].([]any)
	if !ok || len(arr) != 1 {
		return
	}
	blk, ok := arr[0].(map[string]any)
	if ok && blk["type"] == "text" {
		m["system"] = blk["text"]
	}
}

// TestCacheMarkingIsAdditiveProperty is the load-bearing guarantee of prompt
// caching: a cache hint may only ADD cache_control markers, never change anything
// the model is sent. For any system, tools, messages, and hint, the marked request
// must equal the unmarked request once the markers are stripped, the markers must
// be bounded (so a request never exceeds the provider's breakpoint budget), and an
// opaque block (replayed verbatim) must never be marked.
func TestCacheMarkingIsAdditiveProperty(t *testing.T) {
	c := New(secret.New("k"))
	rapid.Check(t, func(rt *rapid.T) {
		sys := rapid.StringMatching(`[a-z ]{0,12}`).Draw(rt, "system")
		nTools := rapid.IntRange(0, 3).Draw(rt, "nTools")
		var toolset []llm.Tool
		for i := range nTools {
			toolset = append(toolset, llm.Tool{
				Name:        rapid.StringMatching(`[a-z]{1,5}`).Draw(rt, "tname"),
				InputSchema: json.RawMessage(`{"type":"object"}`),
			})
			_ = i
		}
		nMsg := rapid.IntRange(0, 4).Draw(rt, "nMsg")
		var msgs []llm.Message
		for i := range nMsg {
			role := llm.RoleUser
			if i%2 == 1 {
				role = llm.RoleAssistant
			}
			nb := rapid.IntRange(1, 3).Draw(rt, "nb")
			var blocks []llm.Block
			for range nb {
				switch rapid.IntRange(0, 2).Draw(rt, "kind") {
				case 0:
					blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: rapid.StringMatching(`[a-z ]{0,8}`).Draw(rt, "text")})
				case 1:
					blocks = append(blocks, llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
						ID: rapid.StringMatching(`[a-z0-9]{1,4}`).Draw(rt, "id"), Name: "echo", Input: json.RawMessage(`{"x":1}`),
					}})
				default:
					blocks = append(blocks, llm.Block{Kind: llm.KindOpaque, Raw: json.RawMessage(`{"type":"thinking","thinking":"t"}`)})
				}
			}
			msgs = append(msgs, llm.Message{Role: role, Blocks: blocks})
		}
		hint := llm.CacheHint{
			Prefix:         rapid.Bool().Draw(rt, "prefix"),
			StableMessages: rapid.IntRange(0, len(msgs)).Draw(rt, "stable"),
		}

		base := llm.Request{System: sys, Tools: toolset, Messages: msgs}
		hinted := base
		hinted.Cache = hint

		marked := tree(rt, c.buildRequest(hinted))
		plain := tree(rt, c.buildRequest(base))

		// An opaque block is replayed verbatim, so it must never carry a marker. Its
		// raw bytes have no cache_control, so finding one anywhere proves none landed
		// on it; combined with the equality below, opaque content is untouched.
		count := stripCacheControl(marked)
		if count > 2 {
			rt.Fatalf("more than 2 cache markers placed (%d): exceeds the breakpoint budget", count)
		}
		normalizeSystem(marked)
		normalizeSystem(plain)
		if !reflect.DeepEqual(marked, plain) {
			rt.Fatalf("marking changed request content:\n marked-stripped: %#v\n plain:          %#v", marked, plain)
		}
	})
}
