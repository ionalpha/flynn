package externagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Claude drives the claude CLI (Claude Code) as an external agent backend over its
// headless print mode, which streams an episode as JSON lines. Unlike codex, claude has
// no native filesystem-sandbox flag; its native execution surface is locked down by
// tool denial instead. The effector tools (shell, edit, write, web) are denied and the
// permission mode never prompts and never auto-approves, so the only tools the model can
// act through are the bridged ones offered over the loopback MCP bridge. This is
// defense in depth: Flynn's own sandbox spawner is the boundary that actually confines
// the child, and it holds regardless of whether the CLI honors the flags.
type Claude struct {
	// bin is the claude executable name or path; empty uses "claude" on PATH.
	bin string
	// spawner runs the detection probes. It may be nil for an adapter used only to build
	// invocations and parse output (the runner supplies its own spawner for the episode
	// itself); Detect requires it.
	spawner Spawner
}

// NewClaude builds the claude adapter. bin overrides the executable to run (empty uses
// "claude" resolved on PATH); spawner runs the detection probes through the sandbox
// boundary and may be nil when the adapter is used only for Command and Parse.
func NewClaude(bin string, spawner Spawner) *Claude {
	if bin == "" {
		bin = "claude"
	}
	return &Claude{bin: bin, spawner: spawner}
}

// Name identifies the adapter in the model spec and on the run.
func (*Claude) Name() string { return "claude" }

// claudeBridgeName is the MCP server name the bridge is registered under in claude's
// config. claude namespaces the tools it reports as mcp__<server>__<tool>, so a tool
// call is recognized as bridged by this prefix.
const claudeBridgeName = "flynn"

// claudeDeniedTools denies the CLI's entire native tool surface, so the model's only way
// to act is a bridged tool that is governed and recorded at the dispatch waist. This is
// deny-by-default made explicit: the allowlist alone does not deny a native tool (the CLI
// auto-runs some without a prompt even under a strict permission mode), so every native
// tool is named here instead, and a denial outranks any allow a build or config carries.
// The list is a superset on purpose; a name a given build does not know is reported on
// stderr, which the spawner discards, so denying a tool that is not present is harmless
// while denying one that is present is what matters. The native read tools are denied too,
// which closes the unobserved-read gap entirely: with no way to read the workspace or the
// host outside a bridged tool, the harness has no observation the run cannot see. The real
// boundary remains Flynn's sandbox, which contains anything a future build might add that
// is not on this list.
var claudeDeniedTools = []string{
	// Shell and command execution.
	"Bash", "BashOutput", "KillShell", "PowerShell",
	// Native file writes and reads (reads denied too: no unobserved-read gap).
	"Edit", "MultiEdit", "Write", "NotebookEdit", "Read", "Glob", "Grep",
	// Native web reach; egress is already gated, this removes the native fetchers.
	"WebFetch", "WebSearch",
	// Sub-agents, orchestration, and skills: no spinning up its own agents, workflows, or
	// slash commands, which run unobserved work and burn subscription usage.
	"Task", "Workflow", "Skill", "SlashCommand", "TodoWrite",
	// Scheduling, background, messaging, and worktree primitives.
	"ScheduleWakeup", "CronCreate", "CronDelete", "CronList", "Monitor",
	"RemoteTrigger", "SendMessage", "PushNotification", "DesignSync",
	"EnterWorktree", "ExitWorktree", "ReportFindings", "ToolSearch",
	// Its own task tracker, which the run governs rather than the CLI.
	"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
}

// claudeCapabilityNotice is appended to the CLI's system prompt on every run, so the model
// knows up front that it holds only the bridged tools and does not waste a turn reaching
// for a native one it cannot use. It is the stated half of the tool lockdown; the denied
// tool list and the sandbox are the enforced half. Managing memory is called out because a
// confined episode's memory never persists, so any effort spent curating it is wasted.
const claudeCapabilityNotice = "You are running as a governed backend inside Flynn, a sandbox that contains this session. " +
	"Your only usable tools are the ones provided by the Flynn MCP server, whose names begin with mcp__flynn__. " +
	"Use those tools for every action, including reading and writing files. " +
	"You do not have, and must not attempt, any built-in tool: no Bash, PowerShell, or shell; " +
	"no native Read, Write, Edit, or notebook tools; no web fetch or web search; " +
	"no Task or subagents; no Workflow; no Skills or slash commands; " +
	"no scheduling, cron, wakeup, background monitor, messaging, or worktree tools. " +
	"These are removed and denied, and attempting one only wastes a turn. " +
	"Do not read, write, or manage memory of any kind: this session does not persist memory, so managing it is wasted effort. " +
	"If a task cannot be done with the mcp__flynn__ tools, say so plainly rather than reaching for a native tool."

// Detect probes that claude is installed, logged in on a subscription, and new enough to
// be constrained to the bridge. It runs claude's own version, help, and auth-status
// probes and never starts an episode. A missing binary or a build without the lockdown
// knobs (headless print, the stream-json event format, the HTTP MCP client, and the tool
// and permission controls) is a hard refusal, so the driver stops rather than running
// claude with unattested effects; a healthy but logged-out CLI is a recoverable
// onboarding prompt.
func (c *Claude) Detect(ctx context.Context) (Readiness, error) {
	if c.spawner == nil {
		return Readiness{Refuse: true, Reason: "claude detection needs a process spawner"}, nil
	}
	version, err := c.spawner.Probe(ctx, c.bin, "--version")
	if err != nil {
		// A failed probe is unreadiness, not a Go error: report an actionable reason so the
		// caller can onboard rather than crash. A CLI on disk that would not run says
		// something different from one that is absent, so the two are reported apart.
		if _, statErr := os.Stat(c.bin); statErr == nil {
			return Readiness{Reason: fmt.Sprintf("claude is installed at %s but the confined child could not run it: %v", c.bin, err)}, nil //nolint:nilerr // an unrunnable CLI is unreadiness, not a Go error
		}
		return Readiness{Reason: "claude CLI not found on PATH; install Claude Code to use the claude backend"}, nil //nolint:nilerr // probe failure is reported as unreadiness
	}
	r := Readiness{Available: true, Version: strings.TrimSpace(version)}

	// The lockdown depends on four knobs: headless print, the stream-json event format,
	// the HTTP MCP client, and the tool and permission controls that deny the native
	// effectors. A build missing any of them cannot be constrained to route effects
	// through the bridge, so refuse rather than run with native effects. claude prints all
	// of these on its top-level help.
	help, err := c.spawner.Probe(ctx, c.bin, "--help")
	if err != nil {
		r.Refuse = true
		r.Reason = "could not read claude's help output to confirm the controls the bridge requires; update Claude Code"
		return r, nil //nolint:nilerr // a probe failure is a refusal reported on Readiness, not a Go error
	}
	for _, knob := range []struct{ flag, need string }{
		{"--print", "headless print mode"},
		{"--output-format", "the stream-json event format"},
		{"--mcp-config", "the HTTP MCP client the bridge requires"},
		{"--disallowedTools", "the tool controls that deny native effectors"},
		{"--permission-mode", "the permission controls that stop native approvals"},
	} {
		if !strings.Contains(help, knob.flag) {
			r.Refuse = true
			r.Reason = "this Claude Code build lacks " + knob.need + " (" + knob.flag + "); update Claude Code"
			return r, nil
		}
	}

	// Login state: claude auth status reports it as JSON without a model call, so
	// detection spends no tokens. A logged-out CLI is an onboarding prompt, not a refusal.
	// The backend drives a subscription, never per-token API billing, so a session
	// authenticated for API/console billing is treated as not-yet-onboarded for this
	// backend and pointed at the subscription login rather than run.
	status, err := c.spawner.Probe(ctx, c.bin, "auth", "status", "--json")
	if err != nil {
		r.Reason = "claude is not logged in; run `claude auth login` and sign in to use your subscription"
		return r, nil //nolint:nilerr // not-logged-in is reported on Readiness, not a Go error
	}
	auth := parseClaudeAuth(status)
	if !auth.LoggedIn {
		r.Reason = "claude is not logged in; run `claude auth login` and sign in to use your subscription"
		return r, nil
	}
	if !auth.subscription() {
		r.Reason = "claude is logged in for API billing, not a subscription; run `claude auth login --claudeai` to drive it on your subscription"
		return r, nil
	}
	r.LoggedIn = true
	return r, nil
}

// claudeAuthStatus is the shape of `claude auth status --json`. Only the fields the
// readiness check needs are decoded.
type claudeAuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	SubscriptionType string `json:"subscriptionType"`
}

// subscription reports whether the logged-in session is a Claude subscription rather
// than API/console billing. The subscription session reports its auth method as the
// consumer product and carries a subscription tier; an API-key or console session
// reports neither, and running headless on it would bill per token.
func (a claudeAuthStatus) subscription() bool {
	return a.AuthMethod == "claude.ai" || a.SubscriptionType != ""
}

// parseClaudeAuth decodes an auth-status probe. A body that does not decode reports as
// not-logged-in, so a changed or unexpected status format onboards the user rather than
// crashing the run.
func parseClaudeAuth(status string) claudeAuthStatus {
	var a claudeAuthStatus
	_ = json.Unmarshal([]byte(strings.TrimSpace(status)), &a)
	return a
}

// Command builds the claude headless invocation for one episode. It runs in print mode
// with the stream-json event format, points claude's MCP client at the loopback bridge
// over HTTP with the bearer token carried in the environment (not on the command line),
// and locks the native surface down by tool denial: the effector tools are denied and
// the permission mode never prompts and never auto-approves, so effects must reach the
// workspace through the bridge. The turn is written on stdin; a standing instruction is
// prepended as a lower-authority preamble, since claude's own harness prompt outranks
// anything injected.
func (c *Claude) Command(ep Episode) (Invocation, error) {
	bridge := ep.Bridge
	if bridge.Name == "" {
		bridge.Name = claudeBridgeName
	}

	mcpConfig, err := claudeMCPConfig(bridge)
	if err != nil {
		return Invocation{}, err
	}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "text",
		// stream-json output requires verbose so every event is emitted, not just the last.
		"--verbose",
		// Only the bridge MCP server, never any the user configured on the host, so the
		// child's tool surface is exactly what this run offers it and nothing else.
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		// dontAsk never blocks on a prompt (headless) and, unlike bypassPermissions, does not
		// auto-approve a tool that would otherwise ask. The native surface is denied by the
		// explicit --disallowedTools list below (the allowlist alone does not deny it), and
		// only the bridge server's tools are allowed.
		"--permission-mode", "dontAsk",
		// State the tool lockdown to the model as well as enforcing it, so it does not spend a
		// turn reaching for a native tool it cannot use.
		"--append-system-prompt", claudeCapabilityNotice,
	}
	if ep.Model != "" {
		args = append(args, "--model", ep.Model)
	}
	// Continue the conversation the CLI already holds, rather than opening a new one and
	// replaying a transcript it never wrote. The id came from the CLI's own stream on an
	// earlier episode of this run, so only a session that already ran one has it.
	if ep.Session != "" {
		args = append(args, "--resume", ep.Session)
	}
	// The allow and deny tool lists are variadic and come last, so the parser reads every
	// tool name into the right list and no positional argument follows them. The turn is
	// read from stdin, so there is no prompt argument to be captured by a variadic flag.
	args = append(args, "--allowedTools", "mcp__"+bridge.Name)
	args = append(args, "--disallowedTools")
	args = append(args, claudeDeniedTools...)

	// The run's instructions reach claude as part of the user turn, below its own harness
	// prompt in authority. The turn is read from stdin so a long or multi-line input does
	// not land on the command line.
	stdin := promptLayers{system: ep.System, probes: ep.Probes, input: ep.Input}.render()

	inv := Invocation{
		Path:  c.bin,
		Args:  args,
		Stdin: stdin,
	}
	// The bearer token is passed through the environment and referenced from the MCP
	// config by name, so it stays out of the process table. ANTHROPIC_API_KEY is never
	// added: a set key silently overrides the subscription and bills per token, and the
	// confined child inherits none of the host environment, so the only way it could
	// appear is if this built it, which it must not.
	if bridge.TokenEnv != "" && bridge.Token != "" {
		inv.Env = append(inv.Env, bridge.TokenEnv+"="+bridge.Token)
	}
	return inv, nil
}

// claudeMCPServers is the inline --mcp-config document: one HTTP MCP server, the loopback
// bridge, with the bearer token referenced from the environment rather than written in.
type claudeMCPServers struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

type claudeMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// claudeMCPConfig builds the inline JSON config that points claude's MCP client at the
// bridge over HTTP. The bearer token is referenced as ${TOKENENV}, which claude expands
// from the child's environment at load time, so the token never appears in the argument
// list. With no token env configured (an offline detection build) the header is omitted.
func claudeMCPConfig(bridge Bridge) (string, error) {
	server := claudeMCPServer{Type: "http", URL: bridge.URL}
	if bridge.TokenEnv != "" {
		server.Headers = map[string]string{"Authorization": "Bearer ${" + bridge.TokenEnv + "}"}
	}
	doc := claudeMCPServers{MCPServers: map[string]claudeMCPServer{bridge.Name: server}}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("externagent: build claude mcp config: %w", err)
	}
	return string(b), nil
}

// claudeEvent is the envelope claude --output-format stream-json emits, one JSON object
// per line. Only the fields the projection needs are decoded; the rest is preserved in
// Raw.
type claudeEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	SessionID string          `json:"session_id"`
	Message   *claudeMessage  `json:"message"`
	Usage     *claudeUsage    `json:"usage"`
	Raw       json.RawMessage `json:"-"`
}

// claudeMessage is the model turn carried by an assistant or user event. Its content is a
// list of typed blocks (text, tool_use, tool_result).
type claudeMessage struct {
	Role    string          `json:"role"`
	Content []claudeContent `json:"content"`
}

// claudeContent is one block of a message. type selects which fields are populated: text
// carries Text; tool_use carries Name and Input; tool_result is a bridged call's return,
// recorded independently at the dispatch waist and projected here only as progress.
type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Parse projects one claude stream-json line to typed events. The init and progress
// events become attested progress; an assistant message projects each of its blocks
// (text as attested text, a bridged tool call as a bridge call, a native tool call as a
// native command); a tool result is progress, since the bridged call it answers is
// enforced and recorded at the dispatch waist; the terminal result event becomes the
// episode's final text plus a done event carrying the total usage, or an error event
// when it reports a failure. A line it cannot decode becomes an attested progress event
// carrying the raw line, so nothing is dropped and noise does not end the episode.
func (c *Claude) Parse(line []byte) ([]Event, error) {
	line = trimLine(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// A line that is not JSON is CLI noise (a banner, a warning), kept as attested
		// progress rather than dropped or treated as a fatal parse error.
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: cloneRaw(line)}}, nil //nolint:nilerr // noise is recorded, not fatal
	}
	raw := cloneRaw(line)
	var out []Event
	switch ev.Type {
	case "assistant":
		out = c.projectMessage(ev.Message, raw)
	case "result":
		out = c.projectResult(ev, raw)
	case "error":
		out = []Event{{Kind: EventError, Err: ev.Result, Terminal: terminalClaudeError(ev.Subtype + " " + ev.Result), Tier: TierAttested, Raw: raw}}
	default:
		// system/init, rate_limit_event, a user tool_result, and any unfamiliar type are
		// attested progress: they carry no result of their own, and the enforced side of a
		// bridged call is recorded at the waist, not from the CLI's echo of it.
		out = []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
	}
	// Every line of the stream carries the conversation id, starting with system/init.
	// Stamping it on the projections is what lets a later episode continue this
	// conversation instead of opening a new one.
	return withSession(out, ev.SessionID), nil
}

// projectMessage projects an assistant message's blocks to typed events. Each block is
// its own event so a turn that both speaks and calls a tool is recorded as both. Every
// projection is attested: the CLI is reporting on itself. A bridged call is separately
// enforced at the dispatch waist, which records it independently of the CLI's account.
// The tool_use block is projected once here; the matching tool_result arrives on a later
// user event and is projected as progress, so a single call is not counted twice.
func (c *Claude) projectMessage(msg *claudeMessage, raw json.RawMessage) []Event {
	if msg == nil || len(msg.Content) == 0 {
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
	}
	var out []Event
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out = append(out, Event{Kind: EventText, Text: block.Text, Tier: TierAttested, Raw: raw})
			}
		case "tool_use":
			out = append(out, c.projectToolUse(block, raw))
		}
	}
	if len(out) == 0 {
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
	}
	return out
}

// projectToolUse projects a tool_use block to a bridge call or a native command. A tool
// whose name is namespaced under the bridge server is a bridged call (its bare tool name
// and arguments are kept so a conformance probe can find the nonce it asked the harness
// to echo); everything else, a native effector or a foreign MCP server, is a native
// command the run did not route through the bridge.
func (c *Claude) projectToolUse(block claudeContent, raw json.RawMessage) Event {
	if server, tool, ok := splitMCPToolName(block.Name); ok && server == claudeBridgeName {
		return Event{
			Kind: EventBridgeCall, Tier: TierAttested, Raw: raw,
			Server: server, Tool: tool, Args: block.Input,
		}
	}
	return Event{Kind: EventNativeCommand, Tier: TierAttested, Raw: raw, Command: block.Name}
}

// projectResult projects the terminal result event. A success carries the episode's final
// text and the total usage; a failure (an error subtype or is_error) becomes a terminal
// or transient error. The final text is projected as a text event so it becomes the
// episode's result, since claude writes no last-message file for the runner to read.
func (c *Claude) projectResult(ev claudeEvent, raw json.RawMessage) []Event {
	if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
		msg := ev.Result
		if msg == "" {
			msg = ev.Subtype
		}
		return []Event{{Kind: EventError, Err: msg, Terminal: terminalClaudeError(ev.Subtype + " " + ev.Result), Tier: TierAttested, Raw: raw}}
	}
	done := Event{Kind: EventDone, Tier: TierAttested, Raw: raw}
	if ev.Usage != nil {
		done.Usage = Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
	}
	if ev.Result != "" {
		return []Event{{Kind: EventText, Text: ev.Result, Tier: TierAttested, Raw: raw}, done}
	}
	return []Event{done}
}

// splitMCPToolName splits a claude MCP tool name (mcp__<server>__<tool>) into its server
// and bare tool name. A name that is not MCP-namespaced (a native tool like Bash or Edit)
// reports ok false. The tool name may itself contain the "__" separator, so only the
// first two segments are the prefix and server; the rest is the tool.
func splitMCPToolName(name string) (server, tool string, ok bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	sep := strings.Index(rest, "__")
	if sep <= 0 || sep+2 >= len(rest) {
		return "", "", false
	}
	return rest[:sep], rest[sep+2:], true
}

// terminalClaudeError reports whether an error names a permanent condition (auth, quota,
// a forbidden request) that a retry cannot fix, so the run stops instead of looping. An
// unrecognized error is treated as transient.
func terminalClaudeError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"unauthorized", "invalid_api_key", "invalid api key", "authentication", "insufficient_quota", "quota", "forbidden", "401", "403", "not supported"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

var _ Adapter = (*Claude)(nil)
