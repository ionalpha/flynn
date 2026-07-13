package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpTestEnv points the vault at a sealed file with a known passphrase, so a served
// session assembles its store and signer without touching the OS keychain.
func mcpTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FLYNN_VAULT_FILE", "1")
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "pw")
}

// mcpFrames renders a client's requests as the newline-free JSON stream the server
// decodes off its input.
func mcpFrames(msgs ...string) string { return strings.Join(msgs, "\n") + "\n" }

// mcpReplies decodes the server's newline-delimited JSON-RPC replies, keyed by request id.
func mcpReplies(t *testing.T, raw string) map[float64]map[string]any {
	t.Helper()
	out := map[float64]map[string]any{}
	dec := json.NewDecoder(strings.NewReader(raw))
	for dec.More() {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			t.Fatalf("decoding a server reply: %v (stream: %s)", err, raw)
		}
		id, ok := msg["id"].(float64)
		if !ok {
			t.Fatalf("reply without an id: %v", msg)
		}
		out[id] = msg
	}
	return out
}

// result pulls the JSON-RPC result object out of a reply, failing when the reply
// carries a protocol error instead.
func mcpResult(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	if e, ok := reply["error"]; ok {
		t.Fatalf("expected a result, got a protocol error: %v", e)
	}
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no result object: %v", reply)
	}
	return res
}

func TestRunMCPRequiresTheServeVerb(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"listen"}} {
		if err := runMCP(args, t.TempDir()); err == nil {
			t.Fatalf("runMCP(%v): expected a usage error", args)
		}
	}
}

func TestRunMCPRejectsAnUnknownFlag(t *testing.T) {
	// The flag set prints its own usage; discard it so the test output stays clean.
	if err := runMCP([]string{"serve", "--no-such-flag"}, t.TempDir()); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
}

// TestRunMCPServesUntilTheClientDisconnects exercises the command as wired, with the
// toolset rooted at --workdir. The test binary's stdin is not a terminal, so the client
// side is immediately at EOF and the session ends the way a disconnect ends it.
func TestRunMCPServesUntilTheClientDisconnects(t *testing.T) {
	mcpTestEnv(t)
	// The command speaks on the process's own streams, so stand a disconnected client in
	// for stdin. The tests in this package run sequentially, so the swap is not observable
	// elsewhere, and it is restored before the next test.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	saved := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() {
		os.Stdin = saved
		_ = devnull.Close()
	})
	if err := runMCP([]string{"serve", "--read-only", "--workdir", t.TempDir()}, t.TempDir()); err != nil {
		t.Fatalf("runMCP serve: %v", err)
	}
}

// TestServeMCPHandshakeAndToolCall drives a full client session over pipes: initialize,
// tools/list, then a governed tools/call that writes a file. It locks the contract that
// stdout carries protocol traffic only (diagnostics go to the log stream) and that an
// admitted call really reaches the tool.
func TestServeMCPHandshakeAndToolCall(t *testing.T) {
	mcpTestEnv(t)
	dir := t.TempDir()

	in := mcpFrames(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write","arguments":{"path":"note.txt","content":"hello"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope/unknown"}`,
	)
	var out, logw bytes.Buffer
	if err := serveMCP(context.Background(), t.TempDir(), dir, false, strings.NewReader(in), &out, &logw); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}

	replies := mcpReplies(t, out.String())
	if len(replies) != 4 {
		t.Fatalf("want 4 replies (the notification gets none), got %d: %s", len(replies), out.String())
	}

	// initialize reports the server identity and echoes the client's protocol version.
	init := mcpResult(t, replies[1])
	if got := init["protocolVersion"]; got != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the client's version echoed back", got)
	}
	info, _ := init["serverInfo"].(map[string]any)
	if info["name"] != "flynn" {
		t.Errorf("serverInfo.name = %v, want flynn", info["name"])
	}

	// tools/list exposes the default toolset, read and write among them.
	names := map[string]bool{}
	list := mcpResult(t, replies[2])
	toolDefs, _ := list["tools"].([]any)
	for _, td := range toolDefs {
		d, _ := td.(map[string]any)
		name, _ := d["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"read", "write", "glob", "grep"} {
		if !names[want] {
			t.Errorf("tools/list is missing %q; got %v", want, names)
		}
	}

	// The admitted write really ran, inside the served directory.
	call := mcpResult(t, replies[3])
	if call["isError"] == true {
		t.Fatalf("the write call was refused in read-write mode: %v", call)
	}
	body, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("the tool call did not write the file: %q, %v", body, err)
	}

	// An unknown method is a protocol error, not a tool result.
	if _, ok := replies[4]["error"]; !ok {
		t.Errorf("an unknown method must return a JSON-RPC error, got %v", replies[4])
	}

	// The banner belongs on the log stream; stdout is protocol-only.
	if !strings.Contains(logw.String(), "serving") || !strings.Contains(logw.String(), "read-write") {
		t.Errorf("the log stream should carry the serving banner, got %q", logw.String())
	}
}

// TestServeMCPReadOnlyDeniesWrite locks the governance contract of --read-only: the write
// tool is still listed, but a call to it is denied at the dispatch waist and comes back as
// an ordinary error tool result, with no file written.
func TestServeMCPReadOnlyDeniesWrite(t *testing.T) {
	mcpTestEnv(t)
	dir := t.TempDir()

	in := mcpFrames(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{"path":"denied.txt","content":"x"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	var out, logw bytes.Buffer
	if err := serveMCP(context.Background(), t.TempDir(), dir, true, strings.NewReader(in), &out, &logw); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}

	replies := mcpReplies(t, out.String())
	call := mcpResult(t, replies[1])
	if call["isError"] != true {
		t.Fatalf("a write in read-only mode must come back as an error tool result, got %v", call)
	}
	if _, err := os.Stat(filepath.Join(dir, "denied.txt")); err == nil {
		t.Fatal("the denied write must not have touched the filesystem")
	}
	if _, ok := replies[2]["result"]; !ok {
		t.Errorf("ping must be answered, got %v", replies[2])
	}
	if !strings.Contains(logw.String(), "read-only") {
		t.Errorf("the banner must name the read-only mode, got %q", logw.String())
	}
}

// TestServeMCPEndsOnClientDisconnect checks a client that hangs up with no traffic ends
// the session cleanly rather than erroring out.
func TestServeMCPEndsOnClientDisconnect(t *testing.T) {
	mcpTestEnv(t)
	var out, logw bytes.Buffer
	if err := serveMCP(context.Background(), t.TempDir(), t.TempDir(), false, strings.NewReader(""), &out, &logw); err != nil {
		t.Fatalf("an immediate disconnect must end cleanly, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no requests were sent, so nothing should be written to the protocol stream: %q", out.String())
	}
}
