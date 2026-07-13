package externagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	// Native execution is locked down: read-only sandbox, denied approvals, the rmcp HTTP
	// client that actually authenticates to the bridge, and the built-in tools codex can be
	// told to drop (web search, image viewer) turned off.
	for _, want := range []string{
		"exec", "--json", "--sandbox read-only", "--skip-git-repo-check",
		`approval_policy="never"`, "experimental_use_rmcp_client=true",
		"tools.web_search=false", "tools.view_image=false",
	} {
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
	// The capability notice leads the preamble, ahead of the run's standing instruction,
	// which sits ahead of the objective. So the model reads the tool contract, then the
	// instruction, then the turn.
	wantStdin := codexCapabilityNotice + "\n\nfollow the contract\n\ndo the thing"
	if inv.Stdin != wantStdin {
		t.Errorf("capability notice + system preamble not prepended to the turn:\n got %q\nwant %q", inv.Stdin, wantStdin)
	}
	if i := strings.Index(inv.Stdin, "flynn"); i < 0 || strings.Index(inv.Stdin, "do the thing") < i {
		t.Errorf("capability notice should precede the objective: %q", inv.Stdin)
	}
	// The capability notice must name the bridged flynn tools and forbid managing memory,
	// the same contract the claude notice states, so governance does not depend on which
	// CLI drove the run.
	if !strings.Contains(codexCapabilityNotice, "flynn") || !strings.Contains(strings.ToLower(codexCapabilityNotice), "memory") {
		t.Errorf("the capability notice must name the bridged tools and forbid managing memory")
	}
	if inv.LastMessageFile == "" {
		t.Errorf("no final-message file configured")
	}
}

// TestCodexCommandNoticeWithoutSystem checks the capability notice still leads the turn
// when the run carries no standing instruction, so the tool contract is always stated.
func TestCodexCommandNoticeWithoutSystem(t *testing.T) {
	inv, err := NewCodex("codex", nil).Command(Episode{Input: "just this", Bridge: Bridge{Name: "flynn"}})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := codexCapabilityNotice + "\n\njust this"
	if inv.Stdin != want {
		t.Errorf("notice not prepended with no system instruction:\n got %q\nwant %q", inv.Stdin, want)
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

// TestCodexContinuesItsOwnThread is codex's half of the interactive-session contract: it
// announces the thread it opened on thread.started, and resumes that thread when a later
// episode hands the id back. The exec flags must survive the resume subcommand, or the
// continued turn would run unconstrained.
func TestCodexContinuesItsOwnThread(t *testing.T) {
	c := NewCodex("codex", nil)

	started := mustParseOne(t, c, `{"type":"thread.started","thread_id":"th-7"}`)
	if started.Session != "th-7" {
		t.Fatalf("thread.started did not report the thread id: %+v", started)
	}

	fresh, err := c.Command(Episode{Input: "first turn"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if strings.Contains(strings.Join(fresh.Args, " "), "resume") {
		t.Errorf("a first episode must not resume anything: %v", fresh.Args)
	}

	next, err := c.Command(Episode{Input: "second turn", Session: "th-7"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(next.Args, " ")
	if !strings.Contains(joined, "exec resume th-7") {
		t.Errorf("a later episode must continue the CLI's thread: %v", next.Args)
	}
	// The lockdown is not optional on a resumed turn: the same sandbox, the same denied
	// approvals, the same bridge. A resumed episode that lost these would run natively.
	for _, want := range []string{"--json", "--sandbox read-only", "approval_policy=\"never\""} {
		if !strings.Contains(joined, want) {
			t.Errorf("resumed episode lost %q from its lockdown: %v", want, next.Args)
		}
	}
}

// TestCodexName pins the identifier the codex loop is selected by in the model spec and
// recorded under on the run. It is a wire contract on both ends, so it is pinned.
func TestCodexName(t *testing.T) {
	if got := NewCodex("", nil).Name(); got != "codex" {
		t.Errorf("Name() = %q, want codex", got)
	}
}

// codexProbes is a healthy set of codex probe answers: a version, the exec help carrying
// the sandbox and JSON-stream controls, the mcp help carrying the streamable-HTTP client,
// and a logged-in status.
type codexProbes struct {
	version, execHelp, mcpHelp, login string
	versionErr, loginErr              error
}

// script answers detection probes from the set, keyed on the probe's arguments.
func (p codexProbes) script() func(args []string) (string, error) {
	return func(args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch joined {
		case "--version":
			return p.version, p.versionErr
		case "exec --help":
			return p.execHelp, nil
		case "mcp add --help":
			return p.mcpHelp, nil
		case "login status":
			return p.login, p.loginErr
		default:
			return "", nil
		}
	}
}

// healthyCodex is the probe set of a codex build new enough to be constrained to the
// bridge, logged in on a subscription.
func healthyCodex() codexProbes {
	return codexProbes{
		version:  "codex-cli 0.88.0",
		execHelp: "Usage: codex exec\n  --json\n  --sandbox <mode>\n  --output-last-message <file>\n",
		mcpHelp:  "Usage: codex mcp add\n  --url <url>\n  --bearer-token-env-var <var>\n",
		login:    "Logged in using ChatGPT",
	}
}

// TestCodexDetectRefusalAndOnboarding walks every outcome detection can reach without a
// real CLI. The distinction the cases pin is refusal versus onboarding: a build that cannot
// be constrained to route its effects through the bridge is a hard refusal (running it
// would produce a record that looks governed while nothing crossed the waist), while a
// healthy but logged-out CLI is a recoverable prompt the user can act on.
func TestCodexDetectRefusalAndOnboarding(t *testing.T) {
	healthy := healthyCodex()
	cases := []struct {
		name             string
		probes           codexProbes
		wantAvailable    bool
		wantRefuse       bool
		wantLoggedIn     bool
		wantReasonSubstr string
	}{
		{
			name: "ready on a subscription", probes: healthy,
			wantAvailable: true, wantLoggedIn: true,
		},
		{
			name:          "refuse a build without the exec sandbox control",
			probes:        codexProbes{version: healthy.version, execHelp: "Usage: codex exec\n  --json\n", mcpHelp: healthy.mcpHelp, login: healthy.login},
			wantAvailable: true, wantRefuse: true, wantReasonSubstr: "--sandbox",
		},
		{
			name:          "refuse a build without the json event stream",
			probes:        codexProbes{version: healthy.version, execHelp: "Usage: codex exec\n  --sandbox <mode>\n", mcpHelp: healthy.mcpHelp, login: healthy.login},
			wantAvailable: true, wantRefuse: true, wantReasonSubstr: "--json",
		},
		{
			name:          "refuse a build without the streamable-http mcp client",
			probes:        codexProbes{version: healthy.version, execHelp: healthy.execHelp, mcpHelp: "Usage: codex mcp add\n  --command <cmd>\n", login: healthy.login},
			wantAvailable: true, wantRefuse: true, wantReasonSubstr: "streamable-HTTP MCP client",
		},
		{
			// The real CLI exits non-zero when it holds no usable credentials, and that is what
			// detection reads: a status probe that fails is a logged-out CLI, not a broken one.
			name:          "onboard a logged-out cli",
			probes:        codexProbes{version: healthy.version, execHelp: healthy.execHelp, mcpHelp: healthy.mcpHelp, loginErr: errors.New("exit status 1")},
			wantAvailable: true, wantReasonSubstr: "codex login",
		},
		{
			name:          "onboard a cli whose status names no session",
			probes:        codexProbes{version: healthy.version, execHelp: healthy.execHelp, mcpHelp: healthy.mcpHelp, login: "no credentials found"},
			wantAvailable: true, wantReasonSubstr: "codex login",
		},
		{
			// The affirmative phrase is a substring of its own negation, so a build that
			// reports "Not logged in" and exits zero must still be read as logged out.
			// Detection cannot lean on the exit code to tell these apart.
			name:          "onboard a cli that denies a session and exits zero",
			probes:        codexProbes{version: healthy.version, execHelp: healthy.execHelp, mcpHelp: healthy.mcpHelp, login: "Not logged in"},
			wantAvailable: true, wantReasonSubstr: "codex login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewCodex("codex", scriptedSpawner{probe: tc.probes.script()}).Detect(context.Background())
			if err != nil {
				t.Fatalf("Detect returned an error (it must report unreadiness, not error): %v", err)
			}
			if r.Available != tc.wantAvailable {
				t.Errorf("Available = %v, want %v", r.Available, tc.wantAvailable)
			}
			if r.Refuse != tc.wantRefuse {
				t.Errorf("Refuse = %v, want %v (reason %q)", r.Refuse, tc.wantRefuse, r.Reason)
			}
			if r.LoggedIn != tc.wantLoggedIn {
				t.Errorf("LoggedIn = %v, want %v (reason %q)", r.LoggedIn, tc.wantLoggedIn, r.Reason)
			}
			if r.Ready() != (tc.wantAvailable && tc.wantLoggedIn && !tc.wantRefuse) {
				t.Errorf("Ready() = %v is inconsistent with %+v", r.Ready(), r)
			}
			if tc.wantReasonSubstr != "" && !strings.Contains(r.Reason, tc.wantReasonSubstr) {
				t.Errorf("reason %q does not mention %q", r.Reason, tc.wantReasonSubstr)
			}
			if !r.Ready() && r.Reason == "" {
				t.Error("an unready CLI must carry an actionable reason")
			}
		})
	}
}

// TestCodexDetectWithoutSpawnerRefuses proves an adapter built for Command and Parse alone
// refuses detection rather than reporting a CLI it never probed as ready.
func TestCodexDetectWithoutSpawnerRefuses(t *testing.T) {
	r, err := NewCodex("codex", nil).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !r.Refuse || r.Ready() {
		t.Errorf("detection with no spawner must refuse, got %+v", r)
	}
}

// TestCodexDetectSeparatesMissingFromUnrunnable proves the two failures a version probe can
// report are onboarded differently. A CLI that is absent must be installed; a CLI that is on
// disk but that the confined child could not execute needs the confinement fixed, not
// another install. Telling the user to install what they already have sends them the wrong
// way, so the reasons are distinct.
func TestCodexDetectSeparatesMissingFromUnrunnable(t *testing.T) {
	failing := codexProbes{versionErr: errors.New("exec format error")}

	absent := filepath.Join(t.TempDir(), "codex")
	r, err := NewCodex(absent, scriptedSpawner{probe: failing.script()}).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if r.Available || !strings.Contains(r.Reason, "not found on PATH") {
		t.Errorf("an absent CLI must be onboarded as not installed, got %+v", r)
	}

	onDisk := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(onDisk, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = NewCodex(onDisk, scriptedSpawner{probe: failing.script()}).Detect(context.Background())
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

// TestCodexProjectItemUnknownShapesAreProgress proves a completed item the projection does
// not recognize, and an item that is missing entirely, are recorded as attested progress
// rather than dropped or mistaken for a tool call. Nothing the harness said is lost, and
// nothing it did not say is invented.
func TestCodexProjectItemUnknownShapesAreProgress(t *testing.T) {
	c := NewCodex("", nil)
	for _, line := range []string{
		`{"type":"item.completed"}`,                                           // no item at all
		`{"type":"item.completed","item":{"type":"reasoning","text":"x"}}`,    // an item kind with no typed projection
		`{"type":"item.completed","item":{"type":"agent_message","text":""}}`, // an empty assistant message
	} {
		ev := mustParseOne(t, c, line)
		if ev.Kind != EventProgress || ev.Tier != TierAttested {
			t.Errorf("Parse(%s) = %+v, want attested progress", line, ev)
		}
		if len(ev.Raw) == 0 {
			t.Errorf("Parse(%s) dropped the harness's own line", line)
		}
	}
}

// TestCodexStreamsAnAssistantMessageAsItAppears proves the live trace does not have to wait
// for the turn to end: an assistant message is projected to text the moment codex starts it.
// The completion of that same item is projected once too, and no other lifecycle line
// produces a text event, so streaming the message early does not count it twice.
func TestCodexStreamsAnAssistantMessageAsItAppears(t *testing.T) {
	c := NewCodex("", nil)

	started := mustParseOne(t, c, `{"type":"item.started","item":{"id":"i1","type":"agent_message","text":"here it comes"}}`)
	if started.Kind != EventText || started.Text != "here it comes" {
		t.Errorf("an assistant message must stream as it appears: %+v", started)
	}

	// An item.updated for the same message is progress, not a second text event, and neither
	// is a started item of any other kind.
	for _, line := range []string{
		`{"type":"item.updated","item":{"id":"i1","type":"agent_message","text":"here it comes"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"command_execution","command":"ls","status":"in_progress"}}`,
		`{"type":"item.started","item":{"id":"i3","type":"agent_message","text":""}}`,
	} {
		if ev := mustParseOne(t, c, line); ev.Kind != EventProgress {
			t.Errorf("Parse(%s) = %+v, want progress: only a started assistant message streams text", line, ev)
		}
	}
}
