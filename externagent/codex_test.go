package externagent

import (
	"context"
	"strings"
	"testing"
)

// TestCodexDetect runs the real detection probes. It is written to hold on a host
// with codex installed and on one without (CI), so it asserts the invariants of each
// outcome rather than a fixed result.
func TestCodexDetect(t *testing.T) {
	r, err := NewCodex("", execSpawner{}).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect returned an error (it should report unreadiness, not error): %v", err)
	}
	if !r.Available {
		if r.Reason == "" {
			t.Errorf("an unavailable CLI must carry an actionable reason")
		}
		if r.Ready() {
			t.Errorf("an unavailable CLI cannot be Ready")
		}
		return
	}
	// Installed: it must report a version, and readiness must be internally consistent.
	if r.Version == "" {
		t.Errorf("an available CLI must report a version")
	}
	if r.Refuse && r.Ready() {
		t.Errorf("a refused CLI cannot be Ready")
	}
	if !r.LoggedIn && r.Ready() {
		t.Errorf("a logged-out CLI cannot be Ready")
	}
}

func TestCodexCommandLocksDownAndBridges(t *testing.T) {
	ep := Episode{
		Input:   "do the thing",
		Workdir: "/work/dir",
		Model:   "gpt-5-codex",
		System:  "follow the contract",
		Bridge: Bridge{
			Name:     "flynn",
			URL:      "http://127.0.0.1:54321/mcp",
			Token:    "tok-abc",
			TokenEnv: "FLYNN_MCP_TOKEN",
		},
	}
	inv, err := NewCodex("codex", nil).Command(ep)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(inv.Args, " ")

	// Native execution is locked down.
	for _, want := range []string{"exec", "--json", "--sandbox read-only", "--skip-git-repo-check", `approval_policy="never"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("invocation missing lockdown %q in: %s", want, joined)
		}
	}
	// The bridge is registered as a streamable-HTTP MCP server with the token in the
	// environment, not on the command line.
	if !strings.Contains(joined, `mcp_servers.flynn.url="http://127.0.0.1:54321/mcp"`) {
		t.Errorf("bridge url not configured: %s", joined)
	}
	if !strings.Contains(joined, `mcp_servers.flynn.bearer_token_env_var="FLYNN_MCP_TOKEN"`) {
		t.Errorf("bridge token env not configured: %s", joined)
	}
	if strings.Contains(joined, "tok-abc") {
		t.Errorf("bearer token must not appear on the command line: %s", joined)
	}
	if !containsEnv(inv.Env, "FLYNN_MCP_TOKEN=tok-abc") {
		t.Errorf("bearer token not passed through the environment: %v", inv.Env)
	}
	// Workdir and model are set; the turn is read from stdin with the system preamble.
	if !strings.Contains(joined, "-C /work/dir") || !strings.Contains(joined, "-m gpt-5-codex") {
		t.Errorf("workdir/model not set: %s", joined)
	}
	if inv.Args[len(inv.Args)-1] != "-" {
		t.Errorf("turn should be read from stdin (last arg '-'): %s", joined)
	}
	if inv.Stdin != "follow the contract\n\ndo the thing" {
		t.Errorf("system preamble not prepended to the turn: %q", inv.Stdin)
	}
	if inv.LastMessageFile == "" {
		t.Errorf("no final-message file configured")
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestCodexParseRealFailureEvents feeds the exact event lines a real codex 0.88.0
// exec run emits when a turn fails, and asserts the typed projection.
func TestCodexParseRealFailureEvents(t *testing.T) {
	c := NewCodex("", nil)

	prog := mustParseOne(t, c, `{"type":"thread.started","thread_id":"019f367f-1631-7b70-9e64-ecaa3abe55ea"}`)
	if prog.Kind != EventProgress || prog.Tier != TierAttested {
		t.Errorf("thread.started should be attested progress, got %+v", prog)
	}
	if turn := mustParseOne(t, c, `{"type":"turn.started"}`); turn.Kind != EventProgress {
		t.Errorf("turn.started should be progress, got %+v", turn)
	}

	errLine := `{"type":"error","message":"{\"detail\":\"The 'gpt-5' model is not supported when using Codex with a ChatGPT account.\"}"}`
	e := mustParseOne(t, c, errLine)
	if e.Kind != EventError {
		t.Fatalf("error line should be an error event, got %+v", e)
	}
	if !e.Terminal {
		t.Errorf(`"not supported" should be a terminal error`)
	}
	if !strings.Contains(e.Err, "not supported") {
		t.Errorf("error message not carried: %q", e.Err)
	}

	failLine := `{"type":"turn.failed","error":{"message":"boom: unauthorized"}}`
	f := mustParseOne(t, c, failLine)
	if f.Kind != EventError || !f.Terminal || !strings.Contains(f.Err, "unauthorized") {
		t.Errorf("turn.failed not projected correctly: %+v", f)
	}
}

func TestCodexParseCompletionTextAndNoise(t *testing.T) {
	c := NewCodex("", nil)

	done := mustParseOne(t, c, `{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":34}}`)
	if done.Kind != EventDone || done.Usage.InputTokens != 120 || done.Usage.OutputTokens != 34 {
		t.Errorf("turn.completed usage not projected: %+v", done)
	}

	text := mustParseOne(t, c, `{"type":"item.completed","item":{"type":"agent_message","text":"the answer"}}`)
	if text.Kind != EventText || text.Text != "the answer" {
		t.Errorf("agent_message not projected to text: %+v", text)
	}

	// A transient error is not marked terminal.
	tr := mustParseOne(t, c, `{"type":"error","message":"stream disconnected, retrying"}`)
	if tr.Kind != EventError || tr.Terminal {
		t.Errorf("unrecognized error should be transient: %+v", tr)
	}

	// A non-JSON line is kept as attested progress, not an error.
	noise := mustParseOne(t, c, `codex: warming up...`)
	if noise.Kind != EventProgress || noise.Tier != TierAttested {
		t.Errorf("noise line should be attested progress: %+v", noise)
	}

	// An empty line yields nothing.
	if evs, err := c.Parse([]byte("   ")); err != nil || len(evs) != 0 {
		t.Errorf("empty line should yield no events, got %v (err %v)", evs, err)
	}
}

func mustParseOne(t *testing.T, c *Codex, line string) Event {
	t.Helper()
	evs, err := c.Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse(%s): %v", line, err)
	}
	if len(evs) != 1 {
		t.Fatalf("Parse(%s): want 1 event, got %d", line, len(evs))
	}
	return evs[0]
}
