package externagent

import (
	"context"
	"strings"
	"testing"
)

// scriptedSpawner answers detection probes from a script keyed on the probe's arguments,
// so a refusal or onboarding path can be exercised without a real CLI. Start is unused by
// detection tests.
type scriptedSpawner struct {
	probe func(args []string) (string, error)
}

func (s scriptedSpawner) Probe(_ context.Context, _ string, args ...string) (string, error) {
	return s.probe(args)
}

func (s scriptedSpawner) Start(context.Context, Episode, Invocation) (Process, error) {
	return nil, nil
}

// claudeHelp is help output carrying every lockdown knob detection checks for.
const claudeHelp = `Usage: claude [options]
  -p, --print
  --output-format <format>
  --input-format <format>
  --mcp-config <configs...>
  --strict-mcp-config
  --disallowedTools <tools...>
  --allowedTools <tools...>
  --permission-mode <mode>
`

// TestClaudeDetect runs the real detection probes. It is written to hold on a host with
// claude installed and on one without (CI), so it asserts the invariants of each outcome
// rather than a fixed result.
func TestClaudeDetect(t *testing.T) {
	r, err := NewClaude("", execSpawner{}).Detect(context.Background())
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

func TestClaudeDetectRefusalAndOnboarding(t *testing.T) {
	loggedIn := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`

	// script builds a probe responder: version and help are healthy unless overridden,
	// and auth status returns the given body.
	script := func(help, auth string) func(args []string) (string, error) {
		return func(args []string) (string, error) {
			switch {
			case len(args) == 1 && args[0] == "--version":
				return "2.1.207 (Claude Code)", nil
			case len(args) == 1 && args[0] == "--help":
				return help, nil
			case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
				return auth, nil
			default:
				return "", nil
			}
		}
	}

	cases := []struct {
		name             string
		help, auth       string
		wantRefuse       bool
		wantLoggedIn     bool
		wantReasonSubstr string
	}{
		{
			name: "ready on a subscription", help: claudeHelp, auth: loggedIn,
			wantLoggedIn: true,
		},
		{
			name: "refuse a build missing the mcp client",
			help: strings.ReplaceAll(claudeHelp, "--mcp-config <configs...>", ""), auth: loggedIn,
			wantRefuse: true, wantReasonSubstr: "--mcp-config",
		},
		{
			name: "refuse a build missing the permission controls",
			help: strings.ReplaceAll(claudeHelp, "--permission-mode <mode>", ""), auth: loggedIn,
			wantRefuse: true, wantReasonSubstr: "--permission-mode",
		},
		{
			name: "onboard a logged-out cli", help: claudeHelp,
			auth:             `{"loggedIn":false}`,
			wantReasonSubstr: "not logged in",
		},
		{
			name: "onboard a console-billed session (not a subscription)", help: claudeHelp,
			auth:             `{"loggedIn":true,"authMethod":"console"}`,
			wantReasonSubstr: "subscription",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := scriptedSpawner{probe: script(tc.help, tc.auth)}
			r, err := NewClaude("claude", sp).Detect(context.Background())
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if r.Refuse != tc.wantRefuse {
				t.Errorf("Refuse = %v, want %v (reason %q)", r.Refuse, tc.wantRefuse, r.Reason)
			}
			if r.LoggedIn != tc.wantLoggedIn {
				t.Errorf("LoggedIn = %v, want %v (reason %q)", r.LoggedIn, tc.wantLoggedIn, r.Reason)
			}
			if tc.wantLoggedIn && !r.Ready() {
				t.Errorf("a logged-in, constrainable CLI should be Ready: %+v", r)
			}
			if tc.wantReasonSubstr != "" && !strings.Contains(r.Reason, tc.wantReasonSubstr) {
				t.Errorf("reason %q does not mention %q", r.Reason, tc.wantReasonSubstr)
			}
		})
	}
}

func TestClaudeCommandLocksDownAndBridges(t *testing.T) {
	ep := Episode{
		Input:   "do the thing",
		Workdir: "/work/dir",
		Model:   "claude-opus-4-8",
		System:  "follow the contract",
		Bridge: Bridge{
			Name:     "flynn",
			URL:      "http://127.0.0.1:54321/mcp",
			Token:    "tok-abc",
			TokenEnv: "FLYNN_MCP_TOKEN",
		},
	}
	inv, err := NewClaude("claude", nil).Command(ep)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(inv.Args, " ")

	// Headless print with the stream-json event format.
	for _, want := range []string{"--print", "--output-format stream-json", "--input-format text", "--verbose"} {
		if !strings.Contains(joined, want) {
			t.Errorf("invocation missing headless knob %q in: %s", want, joined)
		}
	}
	// Native execution is locked down by tool denial and a non-prompting, deny-by-default
	// permission mode; only the bridge server's tools are allowed.
	for _, want := range []string{"--permission-mode dontAsk", "--strict-mcp-config", "--allowedTools mcp__flynn", "--disallowedTools"} {
		if !strings.Contains(joined, want) {
			t.Errorf("invocation missing lockdown %q in: %s", want, joined)
		}
	}
	for _, denied := range claudeDeniedTools {
		if !containsArg(inv.Args, denied) {
			t.Errorf("native effector %q not denied: %s", denied, joined)
		}
	}
	// The orchestration and scheduling primitives the model must not spin up are denied, not
	// just the shell and file effectors.
	for _, want := range []string{"Bash", "Task", "Workflow", "Skill", "ScheduleWakeup", "CronCreate", "PowerShell", "Read"} {
		if !containsArg(claudeDeniedTools, want) {
			t.Errorf("%q must be in the denied tool set", want)
		}
	}
	// The tool lockdown is stated to the model in the system prompt as well as enforced, and
	// it tells the model not to manage memory.
	if !containsArg(inv.Args, "--append-system-prompt") {
		t.Errorf("a capability notice must be appended to the system prompt: %s", joined)
	}
	if !strings.Contains(claudeCapabilityNotice, "mcp__flynn__") || !strings.Contains(strings.ToLower(claudeCapabilityNotice), "memory") {
		t.Errorf("the capability notice must name the bridged tools and forbid managing memory")
	}
	// The MCP client points at the bridge over HTTP with the token referenced from the
	// environment, never written into the config or the argument list.
	if !strings.Contains(joined, `"type":"http"`) || !strings.Contains(joined, `"url":"http://127.0.0.1:54321/mcp"`) {
		t.Errorf("bridge not configured as an HTTP MCP server: %s", joined)
	}
	if !strings.Contains(joined, `Bearer ${FLYNN_MCP_TOKEN}`) {
		t.Errorf("bridge token should be referenced from the environment in the config: %s", joined)
	}
	if strings.Contains(joined, "tok-abc") {
		t.Errorf("bearer token must not appear on the command line: %s", joined)
	}
	if !containsEnv(inv.Env, "FLYNN_MCP_TOKEN=tok-abc") {
		t.Errorf("bearer token not passed through the environment: %v", inv.Env)
	}
	// The subscription is the billing path; an API key would silently override it, so it
	// must never be set anywhere in the invocation.
	for _, e := range inv.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("an api key must never be injected into the child: %q", e)
		}
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY") {
		t.Errorf("no api key should appear in the argument list: %s", joined)
	}
	// The model is set and the turn is read from stdin with the system preamble.
	if !strings.Contains(joined, "--model claude-opus-4-8") {
		t.Errorf("model not set: %s", joined)
	}
	if inv.Stdin != "follow the contract\n\ndo the thing" {
		t.Errorf("system preamble not prepended to the turn: %q", inv.Stdin)
	}
	// The variadic tool lists come last, so no positional prompt argument trails them.
	if last := inv.Args[len(inv.Args)-1]; !containsArg(claudeDeniedTools, last) {
		t.Errorf("a variadic tool list should be last, got trailing arg %q", last)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestClaudeParseInitAndText projects the opening events of a real run: the init and
// rate-limit progress lines, then an assistant text block.
func TestClaudeParseInitAndText(t *testing.T) {
	c := NewClaude("", nil)

	init := mustParseOneClaude(t, c, `{"type":"system","subtype":"init","session_id":"abc","model":"claude-opus-4-8"}`)
	if init.Kind != EventProgress || init.Tier != TierAttested {
		t.Errorf("init should be attested progress, got %+v", init)
	}
	if rl := mustParseOneClaude(t, c, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`); rl.Kind != EventProgress {
		t.Errorf("rate_limit_event should be progress, got %+v", rl)
	}

	text := mustParseOneClaude(t, c, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the answer"}]}}`)
	if text.Kind != EventText || text.Text != "the answer" {
		t.Errorf("assistant text not projected: %+v", text)
	}
}

// TestClaudeParseToolUse projects a bridged tool call and a native one, and confirms the
// matching tool_result is progress, not a second call (no double count).
func TestClaudeParseToolUse(t *testing.T) {
	c := NewClaude("", nil)

	bridged := mustParseOneClaude(t, c, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__flynn__write_file","input":{"path":"a.txt","nonce":"xyz"}}]}}`)
	if bridged.Kind != EventBridgeCall {
		t.Fatalf("bridged tool_use should be a bridge call, got %+v", bridged)
	}
	if bridged.Server != "flynn" || bridged.Tool != "write_file" {
		t.Errorf("bridge call server/tool not split: server=%q tool=%q", bridged.Server, bridged.Tool)
	}
	if !strings.Contains(string(bridged.Args), "xyz") {
		t.Errorf("bridge call args not carried (a probe nonce must survive): %s", bridged.Args)
	}

	native := mustParseOneClaude(t, c, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"ls"}}]}}`)
	if native.Kind != EventNativeCommand || native.Command != "Bash" {
		t.Errorf("native tool_use should be a native command, got %+v", native)
	}

	// A foreign MCP server is not the bridge, so it is native/foreign, not counted as bridged.
	foreign := mustParseOneClaude(t, c, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"mcp__other__do","input":{}}]}}`)
	if foreign.Kind != EventNativeCommand {
		t.Errorf("a foreign MCP server should not count as a bridge call: %+v", foreign)
	}

	// The tool_result the bridged call produces arrives as a user event; it is progress,
	// so the one call is not counted twice.
	result := mustParseOneClaude(t, c, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`)
	if result.Kind != EventProgress {
		t.Errorf("a tool_result should project as progress, not a second call: %+v", result)
	}
}

// TestClaudeParseResult projects the terminal result: a success carries the final text
// and the total usage; an error subtype is terminal.
func TestClaudeParseResult(t *testing.T) {
	c := NewClaude("", nil)

	evs, err := c.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"final answer","usage":{"input_tokens":120,"output_tokens":34}}`))
	if err != nil {
		t.Fatalf("Parse result: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("a successful result should project text and done, got %d events", len(evs))
	}
	if evs[0].Kind != EventText || evs[0].Text != "final answer" {
		t.Errorf("result text not projected as the final message: %+v", evs[0])
	}
	if evs[1].Kind != EventDone || evs[1].Usage.InputTokens != 120 || evs[1].Usage.OutputTokens != 34 {
		t.Errorf("result usage not projected on done: %+v", evs[1])
	}

	// An error result is terminal for auth/quota and carries the reason.
	fail := mustParseOneClaude(t, c, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"unauthorized: please log in"}`)
	if fail.Kind != EventError || !fail.Terminal || !strings.Contains(fail.Err, "unauthorized") {
		t.Errorf("error result not projected as a terminal error: %+v", fail)
	}

	// A transient failure is not marked terminal.
	tr := mustParseOneClaude(t, c, `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"reached the turn limit"}`)
	if tr.Kind != EventError || tr.Terminal {
		t.Errorf("a non-permanent error should be transient: %+v", tr)
	}
}

func TestClaudeParseNoiseAndEmpty(t *testing.T) {
	c := NewClaude("", nil)

	noise := mustParseOneClaude(t, c, `Some non-JSON banner line`)
	if noise.Kind != EventProgress || noise.Tier != TierAttested {
		t.Errorf("noise line should be attested progress: %+v", noise)
	}
	if evs, err := c.Parse([]byte("   ")); err != nil || len(evs) != 0 {
		t.Errorf("empty line should yield no events, got %v (err %v)", evs, err)
	}
}

func TestSplitMCPToolName(t *testing.T) {
	cases := []struct {
		name                 string
		wantServer, wantTool string
		wantOK               bool
	}{
		{"mcp__flynn__write_file", "flynn", "write_file", true},
		{"mcp__flynn__deep__tool", "flynn", "deep__tool", true}, // tool name may contain the separator
		{"Bash", "", "", false},
		{"mcp__flynn__", "", "", false}, // no tool name
		{"mcp__", "", "", false},        // no server
		{"", "", "", false},
	}
	for _, tc := range cases {
		server, tool, ok := splitMCPToolName(tc.name)
		if ok != tc.wantOK || server != tc.wantServer || tool != tc.wantTool {
			t.Errorf("splitMCPToolName(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.name, server, tool, ok, tc.wantServer, tc.wantTool, tc.wantOK)
		}
	}
}

func mustParseOneClaude(t *testing.T, c *Claude, line string) Event {
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
