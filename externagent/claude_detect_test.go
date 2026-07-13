package externagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeName pins the identifier the claude loop is selected by in the model spec and
// recorded under on the run.
func TestClaudeName(t *testing.T) {
	if got := NewClaude("", nil).Name(); got != "claude" {
		t.Errorf("Name() = %q, want claude", got)
	}
}

// TestClaudeDetectWithoutSpawnerRefuses proves an adapter built for Command and Parse alone
// refuses detection rather than reporting a CLI it never probed as ready.
func TestClaudeDetectWithoutSpawnerRefuses(t *testing.T) {
	r, err := NewClaude("claude", nil).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !r.Refuse || r.Ready() {
		t.Errorf("detection with no spawner must refuse, got %+v", r)
	}
}

// TestClaudeDetectSeparatesMissingFromUnrunnable proves the two failures a version probe
// can report are onboarded differently: a CLI that is absent must be installed, while one
// that is on disk but that the confined child could not execute needs the confinement
// fixed. Telling the user to install what they already have sends them the wrong way, so
// the two reasons are distinct.
func TestClaudeDetectSeparatesMissingFromUnrunnable(t *testing.T) {
	failing := scriptedSpawner{probe: func([]string) (string, error) {
		return "", errors.New("permission denied")
	}}

	absent := filepath.Join(t.TempDir(), "claude")
	r, err := NewClaude(absent, failing).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if r.Available || !strings.Contains(r.Reason, "not found on PATH") {
		t.Errorf("an absent CLI must be onboarded as not installed, got %+v", r)
	}

	onDisk := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(onDisk, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = NewClaude(onDisk, failing).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if r.Available || !strings.Contains(r.Reason, "could not run it") {
		t.Errorf("a CLI on disk that would not run must say so, not claim it is uninstalled: %+v", r)
	}
	if !strings.Contains(r.Reason, onDisk) {
		t.Errorf("the reason must name the path that failed, got %q", r.Reason)
	}
}

// TestClaudeDetectUnreadableHelpRefuses proves a CLI whose help output cannot be read is a
// refusal rather than an assumption of health. The help is how detection confirms the build
// carries the controls the lockdown depends on; without it there is no evidence the child
// can be constrained, and running anyway would produce a record that looks governed while
// the native effectors were never denied.
func TestClaudeDetectUnreadableHelpRefuses(t *testing.T) {
	sp := scriptedSpawner{probe: func(args []string) (string, error) {
		if len(args) == 1 && args[0] == "--version" {
			return "2.1.207 (Claude Code)", nil
		}
		return "", errors.New("help exited non-zero")
	}}
	r, err := NewClaude("claude", sp).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !r.Available {
		t.Fatalf("a CLI that answered its version probe is available: %+v", r)
	}
	if !r.Refuse || r.Ready() {
		t.Errorf("an unconfirmable build must be refused, got %+v", r)
	}
	if !strings.Contains(r.Reason, "help") {
		t.Errorf("the reason must say the help could not be read, got %q", r.Reason)
	}
}

// TestClaudeDetectAuthProbeFailureOnboards proves a failing auth-status probe is an
// onboarding prompt and not a refusal: the build is constrainable, the user simply has to
// log in. A refusal here would make an un-onboarded install look like a broken one.
func TestClaudeDetectAuthProbeFailureOnboards(t *testing.T) {
	sp := scriptedSpawner{probe: func(args []string) (string, error) {
		switch {
		case len(args) == 1 && args[0] == "--version":
			return "2.1.207 (Claude Code)", nil
		case len(args) == 1 && args[0] == "--help":
			return claudeHelp, nil
		default:
			return "", errors.New("auth status exited non-zero")
		}
	}}
	r, err := NewClaude("claude", sp).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if r.Refuse {
		t.Errorf("a logged-out CLI is an onboarding prompt, not a refusal: %+v", r)
	}
	if r.LoggedIn || !strings.Contains(r.Reason, "not logged in") {
		t.Errorf("a failed auth probe must onboard the login, got %+v", r)
	}
}

// TestParseClaudeAuthTreatsUnknownShapesAsLoggedOut proves an auth-status body that does not
// decode reports as logged out. A changed or unexpected status format must onboard the user
// rather than crash the run, and must never be mistaken for a live subscription.
func TestParseClaudeAuthTreatsUnknownShapesAsLoggedOut(t *testing.T) {
	for _, body := range []string{"", "not json at all", "{}", `{"loggedIn":false,"authMethod":"claude.ai"}`} {
		if parseClaudeAuth(body).LoggedIn {
			t.Errorf("parseClaudeAuth(%q) reported a logged-in session", body)
		}
	}
	// A subscription is recognized by the consumer auth method or by a tier. An API/console
	// session reports neither, and running it headless would bill per token instead of
	// drawing on the subscription, so it is not treated as one.
	cases := []struct {
		body string
		want bool
	}{
		{`{"loggedIn":true,"authMethod":"claude.ai"}`, true},
		{`{"loggedIn":true,"subscriptionType":"max"}`, true},
		{`{"loggedIn":true,"authMethod":"console"}`, false},
		{`{"loggedIn":true}`, false},
	}
	for _, tc := range cases {
		if got := parseClaudeAuth(tc.body).subscription(); got != tc.want {
			t.Errorf("parseClaudeAuth(%s).subscription() = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestClaudeMCPConfigOmitsTheHeaderWithoutATokenEnv proves the inline MCP config carries an
// Authorization header only when there is an environment variable to read the token from,
// and that the token itself is never written into the document. The config lands on the
// command line, so a token written in rather than referenced would sit in the process table.
func TestClaudeMCPConfigOmitsTheHeaderWithoutATokenEnv(t *testing.T) {
	withEnv, err := claudeMCPConfig(Bridge{Name: "flynn", URL: "http://127.0.0.1:1/mcp", Token: "tok-abc", TokenEnv: "FLYNN_MCP_TOKEN"})
	if err != nil {
		t.Fatalf("claudeMCPConfig: %v", err)
	}
	if !strings.Contains(withEnv, "Bearer ${FLYNN_MCP_TOKEN}") {
		t.Errorf("the token must be referenced from the environment: %s", withEnv)
	}
	if strings.Contains(withEnv, "tok-abc") {
		t.Errorf("the token itself must never be written into the config: %s", withEnv)
	}

	// An offline detection build configures no token env, so there is nothing to reference
	// and no header to send.
	noEnv, err := claudeMCPConfig(Bridge{Name: "flynn", URL: "http://127.0.0.1:1/mcp"})
	if err != nil {
		t.Fatalf("claudeMCPConfig: %v", err)
	}
	if strings.Contains(noEnv, "Authorization") || strings.Contains(noEnv, "headers") {
		t.Errorf("with no token env there is no header to send: %s", noEnv)
	}
}

// TestClaudeProjectsEmptyAndUnknownMessagesAsProgress proves an assistant turn the
// projection finds nothing typed in is still recorded, as attested progress carrying the
// harness's line. A message with no content, one whose only block is empty text, and one
// whose blocks are of a kind with no projection must none of them be dropped.
func TestClaudeProjectsEmptyAndUnknownMessagesAsProgress(t *testing.T) {
	c := NewClaude("", nil)
	for _, line := range []string{
		`{"type":"assistant","message":{"role":"assistant","content":[]}}`,
		`{"type":"assistant","message":{"role":"assistant"}}`,
		`{"type":"assistant"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":""}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`,
	} {
		ev := mustParseOneClaude(t, c, line)
		if ev.Kind != EventProgress || ev.Tier != TierAttested {
			t.Errorf("Parse(%s) = %+v, want attested progress", line, ev)
		}
		if len(ev.Raw) == 0 {
			t.Errorf("Parse(%s) dropped the harness's own line", line)
		}
	}
}

// TestClaudeProjectsATurnThatSpeaksAndActs proves a message carrying both text and a tool
// call is recorded as both events off the one line, so a turn is not reduced to whichever
// block happened to come first.
func TestClaudeProjectsATurnThatSpeaksAndActs(t *testing.T) {
	c := NewClaude("", nil)
	evs, err := c.Parse([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"writing it now"},{"type":"tool_use","name":"mcp__flynn__write","input":{"path":"a.txt"}}]}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("a turn that speaks and acts should project both, got %d events", len(evs))
	}
	if evs[0].Kind != EventText || evs[0].Text != "writing it now" {
		t.Errorf("the spoken block was not projected as text: %+v", evs[0])
	}
	if evs[1].Kind != EventBridgeCall || evs[1].Tool != "write" {
		t.Errorf("the tool call was not projected as a bridge call: %+v", evs[1])
	}
}

// TestClaudeProjectsResultVariants covers the terminal result shapes the runner reads an
// episode's outcome from: a success with no text at all (a done event and nothing to say),
// an error line of its own, and an error result whose only description is its subtype. A
// failure carrying no message would otherwise be recorded with an empty reason, and the run
// would report that the episode failed without saying why.
func TestClaudeProjectsResultVariants(t *testing.T) {
	c := NewClaude("", nil)

	only := mustParseOneClaude(t, c, `{"type":"result","subtype":"success","is_error":false}`)
	if only.Kind != EventDone || only.Usage != (Usage{}) {
		t.Errorf("a success with no text should be a bare done event: %+v", only)
	}

	bare := mustParseOneClaude(t, c, `{"type":"result","subtype":"error_during_execution","is_error":true}`)
	if bare.Kind != EventError || bare.Err != "error_during_execution" {
		t.Errorf("a failure with no message must fall back to its subtype: %+v", bare)
	}

	line := mustParseOneClaude(t, c, `{"type":"error","subtype":"forbidden","result":"the request was forbidden"}`)
	if line.Kind != EventError || !line.Terminal || !strings.Contains(line.Err, "forbidden") {
		t.Errorf("an error line should be a terminal error carrying its reason: %+v", line)
	}
}
