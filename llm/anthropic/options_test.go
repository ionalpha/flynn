package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

// TestWithModel proves the model id is overridable and that an empty override keeps the
// default rather than sending a request with no model.
func TestWithModel(t *testing.T) {
	if got := New(secret.New("k"), WithModel("claude-test-1")).model; got != "claude-test-1" {
		t.Fatalf("model = %q, want the override", got)
	}
	if got := New(secret.New("k"), WithModel("")).model; got != DefaultModel {
		t.Fatalf("an empty model override must keep the default, got %q", got)
	}
}

// TestWithBaseURL proves an override can never downgrade the transport the API key
// travels over: https and loopback http are taken, a plaintext remote endpoint is
// refused and the secure default kept.
func TestWithBaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"https proxy is taken", "https://proxy.example.com", "https://proxy.example.com"},
		{"loopback http is taken", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"plaintext remote is refused", "http://proxy.example.com", defaultBaseURL},
		{"unknown scheme is refused", "ftp://proxy.example.com", defaultBaseURL},
		{"empty keeps the default", "", defaultBaseURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New(secret.New("k"), WithBaseURL(c.url)).baseURL; got != c.want {
				t.Fatalf("baseURL = %q, want %q", got, c.want)
			}
		})
	}
}

// TestWithMaxTokens proves the output ceiling is overridable, that a non-positive value
// is ignored, and that the configured ceiling reaches the wire when a request does not
// set its own.
func TestWithMaxTokens(t *testing.T) {
	if got := New(secret.New("k"), WithMaxTokens(512)).maxTokens; got != 512 {
		t.Fatalf("maxTokens = %d, want 512", got)
	}
	for _, bad := range []int{0, -1} {
		if got := New(secret.New("k"), WithMaxTokens(bad)).maxTokens; got != DefaultMaxTokens {
			t.Fatalf("WithMaxTokens(%d) must keep the default, got %d", bad, got)
		}
	}

	m := &mockTransport{status: 200, respBody: `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`}
	c := clientWith(m, WithMaxTokens(512))
	if _, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")}}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(m.gotBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.MaxTokens != 512 {
		t.Fatalf("max_tokens on the wire = %d, want the configured ceiling", sent.MaxTokens)
	}
}

// TestMapStopReason proves each API stop reason maps onto the port's, with every
// turn-ending reason collapsing to StopEndTurn.
func TestMapStopReason(t *testing.T) {
	cases := map[string]llm.StopReason{
		"tool_use":      llm.StopToolUse,
		"max_tokens":    llm.StopMaxTokens,
		"end_turn":      llm.StopEndTurn,
		"refusal":       llm.StopEndTurn,
		"stop_sequence": llm.StopEndTurn,
		"pause_turn":    llm.StopEndTurn,
		"":              llm.StopEndTurn,
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%q) = %v, want %v", in, got, want)
		}
	}
}
