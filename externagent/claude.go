package externagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Claude drives the claude CLI (Claude Code) in headless print mode, which streams an
// episode as JSON lines. claude has no filesystem-sandbox flag, so its native surface
// is locked down by tool denial: the effector tools are denied and the permission mode
// neither prompts nor auto-approves, leaving only the bridged tools. The sandbox
// spawner confines the child whether or not the CLI honors the flags.
type Claude struct {
	// bin is the executable name or path; empty uses "claude" on PATH.
	bin string
	// spawner runs the detection probes. Detect requires it; Command and Parse do not.
	spawner Spawner
}

// NewClaude builds the claude adapter. Empty bin uses "claude" on PATH; spawner may be
// nil when the adapter is used only for Command and Parse.
func NewClaude(bin string, spawner Spawner) *Claude {
	if bin == "" {
		bin = "claude"
	}
	return &Claude{bin: bin, spawner: spawner}
}

// Name identifies the adapter in the model spec and on the run.
func (*Claude) Name() string { return "claude" }

// Lockdown states what a claude episode leaves the harness holding. Every native
// effector is named on --disallowedTools and the permission mode never prompts or
// auto-approves, so the model is never offered them. --strict-mcp-config loads only
// the run's bridge, over a per-episode home holding just the credentials, so the
// host's MCP servers, hooks, plugins and settings are out of reach. The sandbox
// enforces all of this; the flags spare the model a turn discovering what it cannot use.
func (*Claude) Lockdown() Lockdown {
	return Lockdown{Writes: StripDenied, Commands: StripDenied, HostConfig: StripDenied}
}

// claudeBridgeName is the bridge's MCP server name. claude reports tools as
// mcp__<server>__<tool>, so this prefix marks a call as bridged.
const claudeBridgeName = "flynn"

// claudeDeniedTools names the CLI's entire native tool surface, so the model can act
// only through bridged tools. The allowlist alone does not deny a native tool (the CLI
// auto-runs some even under a strict permission mode), and a denial outranks any allow
// a build or config carries. The list is a superset: an unknown name only produces a
// stderr warning the spawner discards. Read tools are denied too, so the harness has
// no observation the run cannot see. The sandbox contains anything a future build adds.
var claudeDeniedTools = []string{
	// Shell and command execution.
	"Bash", "BashOutput", "KillShell", "PowerShell",
	// Native file writes, and reads, so no read goes unobserved.
	"Edit", "MultiEdit", "Write", "NotebookEdit", "Read", "Glob", "Grep",
	// Native fetchers; egress is gated regardless.
	"WebFetch", "WebSearch",
	// Its own agents, workflows and slash commands, which run unobserved work and burn
	// subscription usage.
	"Task", "Workflow", "Skill", "SlashCommand", "TodoWrite",
	// Scheduling, background, messaging, and worktree primitives.
	"ScheduleWakeup", "CronCreate", "CronDelete", "CronList", "Monitor",
	"RemoteTrigger", "SendMessage", "PushNotification", "DesignSync",
	"EnterWorktree", "ExitWorktree", "ReportFindings", "ToolSearch",
	// Its own task tracker; the run governs tasks.
	"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
}

// claudeCapabilityNotice is appended to the system prompt on every run, so the model
// knows it holds only bridged tools and does not waste a turn on a native one. Memory
// is called out because a confined episode's memory never persists.
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

// Detect checks that claude is installed, signed in on a subscription, and new enough
// to be constrained, using its version, help and auth-status probes. A missing binary
// or a build lacking a lockdown option is a hard refusal; a logged-out CLI is a
// recoverable onboarding prompt.
func (c *Claude) Detect(ctx context.Context) (Readiness, error) {
	version, stop := cliProbe{
		name: "claude", bin: c.bin, spawner: c.spawner,
		notFound: "claude CLI not found on PATH; install Claude Code to use the claude backend",
	}.probeVersion(ctx)
	if stop != nil {
		return *stop, nil
	}
	r := Readiness{Available: true, Version: version}

	// The lockdown needs headless print, stream-json output, the HTTP MCP client, and
	// the tool and permission controls; a build missing any cannot be constrained.
	// claude lists all of them on its top-level help.
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

	// claude auth status answers without a model call, so detection spends no tokens.
	// The backend drives a subscription only, so an API or console session is pointed
	// at the subscription login instead of being run on per-token billing.
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

// claudeAuthStatus is the part of `claude auth status --json` readiness needs.
type claudeAuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	SubscriptionType string `json:"subscriptionType"`
}

// subscription reports whether the session is a Claude subscription: it names the
// consumer product as its auth method and carries a tier. An API-key or console
// session has neither, and headless use would bill per token.
func (a claudeAuthStatus) subscription() bool {
	return a.AuthMethod == "claude.ai" || a.SubscriptionType != ""
}

// parseClaudeAuth decodes an auth-status probe. An undecodable body reads as logged
// out, so a changed format onboards the user instead of crashing the run.
func parseClaudeAuth(status string) claudeAuthStatus {
	var a claudeAuthStatus
	_ = json.Unmarshal([]byte(strings.TrimSpace(status)), &a)
	return a
}

// Command builds the headless invocation for one episode: print mode with stream-json
// output, the MCP client pointed at the bridge over HTTP with the token in the
// environment, and the native surface denied. The turn goes on stdin, with the
// standing instruction as a lower-authority preamble.
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
		// stream-json needs verbose to emit every event instead of only the last.
		"--verbose",
		// Only the bridge server, never one configured on the host.
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		// dontAsk never blocks on a prompt and, where bypassPermissions would
		// auto-approve, refuses. The native surface is denied by --disallowedTools below.
		"--permission-mode", "dontAsk",
		// Tell the model about the lockdown so it does not spend a turn on a native tool.
		"--append-system-prompt", claudeCapabilityNotice,
	}
	if ep.Model != "" {
		args = append(args, "--model", ep.Model)
	}
	// Continue the conversation the CLI holds. The id came from the CLI's stream on an
	// earlier episode of this run.
	if ep.Session != "" {
		args = append(args, "--resume", ep.Session)
	}
	// The variadic tool lists come last, so no positional argument is read as a tool
	// name. The turn is on stdin, so no prompt argument follows them either.
	args = append(args, "--allowedTools", "mcp__"+bridge.Name)
	args = append(args, "--disallowedTools")
	args = append(args, claudeDeniedTools...)

	// On stdin so a long or multi-line turn stays off the command line.
	stdin := promptLayers{system: ep.System, probes: ep.Probes, input: ep.Input}.render()

	inv := Invocation{
		Path:  c.bin,
		Args:  args,
		Stdin: stdin,
	}
	// The token goes in the environment, named from the MCP config, to stay out of the
	// process table. ANTHROPIC_API_KEY is never added: a set key overrides the
	// subscription and bills per token, and the child inherits no host environment, so
	// only this code could put it there.
	if bridge.TokenEnv != "" && bridge.Token != "" {
		inv.Env = append(inv.Env, bridge.TokenEnv+"="+bridge.Token)
	}
	return inv, nil
}

// claudeMCPServers is the inline --mcp-config document: the loopback bridge as one
// HTTP MCP server.
type claudeMCPServers struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

type claudeMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// claudeMCPConfig builds the inline MCP config. The token is written as ${TOKENENV},
// which claude expands from the child's environment, so it never appears in the
// arguments. With no token variable (an offline detection build) the header is omitted.
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

// claudeEvent is one line of claude's stream-json output, decoded only as far as the
// projection needs.
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

// claudeMessage is the turn carried by an assistant or user event, as typed blocks
// (text, tool_use, tool_result).
type claudeMessage struct {
	Role    string          `json:"role"`
	Content []claudeContent `json:"content"`
}

// claudeContent is one message block. Type selects the populated fields. A tool_result
// is projected only as progress because the waist records the bridged call.
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

// Parse projects one stream-json line to events. Init and progress events are attested
// progress; each assistant block is its own event; a tool result is progress because
// the waist records the call; the terminal result becomes the final text and a done
// event with total usage, or an error. An undecodable line is attested progress with
// the raw line, so noise is recorded and does not end the episode.
func (c *Claude) Parse(line []byte) ([]Event, error) {
	line = trimLine(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// A non-JSON line is CLI noise (a banner, a warning).
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
		// system/init, rate_limit_event, a user tool_result and unknown types carry no
		// result of their own.
		out = []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
	}
	// Every line carries the conversation id, starting with system/init; a later
	// episode needs it to continue the conversation.
	return withSession(out, ev.SessionID), nil
}

// projectMessage makes one attested event per block, so a turn that speaks and calls a
// tool records both. tool_use is projected here and its tool_result later as progress,
// so one call is counted once.
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

// projectToolUse maps a tool namespaced under the bridge to a bridge call, keeping the
// bare name and arguments so a conformance probe can find its nonce. Anything else, a
// native tool or a foreign MCP server, is a native command.
func (c *Claude) projectToolUse(block claudeContent, raw json.RawMessage) Event {
	if server, tool, ok := splitMCPToolName(block.Name); ok && server == claudeBridgeName {
		return Event{
			Kind: EventBridgeCall, Tier: TierAttested, Raw: raw,
			Server: server, Tool: tool, Args: block.Input,
		}
	}
	return Event{Kind: EventNativeCommand, Tier: TierAttested, Raw: raw, Command: block.Name}
}

// projectResult projects the terminal result: final text and total usage on success, a
// terminal or transient error on failure. The text becomes the episode's result because
// claude writes no last-message file.
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

// splitMCPToolName splits mcp__<server>__<tool>. ok is false for a native tool name.
// The tool name may itself contain "__", so only the first two segments are prefix and
// server.
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

// terminalClaudeError reports whether an error is permanent (auth, quota, a forbidden
// request), so the run stops instead of retrying. Unrecognized errors are transient.
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
