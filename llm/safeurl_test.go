package llm_test

import (
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// TestSafeBaseURL proves the transport rule a credential depends on: https anywhere,
// plaintext only to the loopback host, and nothing else. An empty base URL means "use
// the provider default" and is safe.
func TestSafeBaseURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty means provider default", "", true},
		{"https remote", "https://api.example.com", true},
		{"https with path and port", "https://api.example.com:8443/v1", true},
		{"http to localhost", "http://localhost:8080/v1", true},
		{"http to loopback ipv4", "http://127.0.0.1:11434", true},
		{"http to loopback ipv6", "http://[::1]:11434", true},
		{"http to a remote host", "http://api.example.com", false},
		{"http to a remote ip", "http://203.0.113.10:8080", false},
		{"http to a private lan ip is still remote", "http://192.168.1.5:8080", false},
		{"no host", "https:///v1", false},
		{"relative path", "/v1/messages", false},
		{"unknown scheme", "ftp://example.com", false},
		{"unencrypted websocket", "ws://example.com", false},
		{"unparseable", "https://exa mple.com/\x7f", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := llm.SafeBaseURL(c.raw); got != c.want {
				t.Fatalf("SafeBaseURL(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestTextBuildsSingleBlockMessage proves the ergonomic constructor produces exactly one
// text block in the requested role, the shape every backend encoder expects.
func TestTextBuildsSingleBlockMessage(t *testing.T) {
	m := llm.Text(llm.RoleUser, "hello")
	if m.Role != llm.RoleUser {
		t.Fatalf("role = %q, want user", m.Role)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Kind != llm.KindText || m.Blocks[0].Text != "hello" {
		t.Fatalf("blocks = %+v", m.Blocks)
	}
	if m.TextContent() != "hello" {
		t.Fatalf("TextContent = %q", m.TextContent())
	}
	if got := llm.Text(llm.RoleAssistant, "ack"); got.Role != llm.RoleAssistant {
		t.Fatalf("role = %q, want assistant", got.Role)
	}
}
