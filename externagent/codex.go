package externagent

import (
	"context"
	"encoding/json"
	"strings"
)

// Codex drives the codex CLI in non-interactive exec mode, which streams an episode as
// JSON lines and writes the final message to a file. A read-only native sandbox with
// approvals denied stops the model writing or running a command directly, so its
// effects reach the workspace only through the governed bridge.
type Codex struct {
	// bin is the executable name or path; empty uses "codex" on PATH.
	bin string
	// spawner runs the detection probes. Detect requires it; Command and Parse do not.
	spawner Spawner
}

// NewCodex builds the codex adapter. Empty bin uses "codex" on PATH; spawner may be nil
// when the adapter is used only for Command and Parse.
func NewCodex(bin string, spawner Spawner) *Codex {
	if bin == "" {
		bin = "codex"
	}
	return &Codex{bin: bin, spawner: spawner}
}

// Name identifies the adapter in the model spec and on the run.
func (*Codex) Name() string { return "codex" }

// Lockdown states what a codex episode leaves the harness holding. codex has no flag
// that removes its shell or patch tools, so writes and commands are contained instead
// of denied: the model is offered them, the read-only sandbox makes them fail, and each
// attempt counts as native drift. The host points the child at a CODEX_HOME apart from
// the operator's, with no config.toml, so the operator's MCP servers, model providers
// and settings are out of reach and the only server loaded is the one on the command
// line.
func (*Codex) Lockdown() Lockdown {
	return Lockdown{Writes: StripContained, Commands: StripContained, HostConfig: StripContained}
}

// codexBridgeName is the bridge's MCP server name; codex namespaces tool names with it.
const codexBridgeName = "flynn"

// codexCapabilityNotice leads the episode's preamble on every run, so the model knows
// it holds only bridged tools and does not waste a turn on a built-in one. codex has no
// --append-system-prompt, so the notice rides the user turn, below codex's harness
// prompt; the sandbox enforces what it states. Memory is called out because a confined
// episode's memory never persists.
const codexCapabilityNotice = "You are running as a governed backend inside Flynn, a sandbox that contains this session. " +
	"Your only usable tools are the ones provided by the Flynn MCP server (the server named \"flynn\"). " +
	"Use those tools for every action, including reading and writing files. " +
	"Do not use, and do not rely on, any built-in tool: no shell or command execution, " +
	"no native file edits or patches, no web search, and no image viewer. " +
	"Native writes and commands are blocked by a read-only sandbox with denied approvals, and the other built-ins are turned off, " +
	"so reaching for one only wastes a turn. " +
	"Do not read, write, or manage memory of any kind: this session does not persist memory, so managing it is wasted effort. " +
	"If a task cannot be done with the flynn tools, say so plainly rather than reaching for a built-in tool."

// Detect checks that codex is installed, logged in, and new enough to be constrained,
// using its version, auth and help probes. A missing binary or a build lacking the
// read-only sandbox, the JSON event stream or the streamable-HTTP MCP client is a hard
// refusal; a logged-out CLI is a recoverable onboarding prompt.
func (c *Codex) Detect(ctx context.Context) (Readiness, error) {
	version, stop := cliProbe{
		name: "codex", bin: c.bin, spawner: c.spawner,
		notFound: "codex CLI not found on PATH; install it to use the codex backend",
	}.probeVersion(ctx)
	if stop != nil {
		return *stop, nil
	}
	r := Readiness{Available: true, Version: version}

	// A build missing the read-only sandbox, the JSON stream on exec, or the
	// streamable-HTTP MCP client cannot be constrained.
	execHelp, err := c.spawner.Probe(ctx, c.bin, "exec", "--help")
	if err != nil || !strings.Contains(execHelp, "--json") || !strings.Contains(execHelp, "--sandbox") {
		r.Refuse = true
		r.Reason = "this codex build lacks the exec --json / --sandbox controls the bridge requires; update codex"
		return r, nil //nolint:nilerr // a probe failure is a refusal reported on Readiness, not a Go error
	}
	mcpHelp, err := c.spawner.Probe(ctx, c.bin, "mcp", "add", "--help")
	if err != nil || !strings.Contains(mcpHelp, "--url") {
		r.Refuse = true
		r.Reason = "this codex build lacks the streamable-HTTP MCP client the bridge requires; update codex"
		return r, nil //nolint:nilerr // a probe failure is a refusal reported on Readiness, not a Go error
	}

	// "not logged in" contains "logged in", so a bare Contains reads a logged-out CLI
	// as logged in. The negation is ruled out explicitly so the answer does not rest on
	// the exit code alone.
	status, err := c.spawner.Probe(ctx, c.bin, "login", "status")
	lower := strings.ToLower(status)
	loggedIn := err == nil &&
		strings.Contains(lower, "logged in") &&
		!strings.Contains(lower, "not logged in")
	if !loggedIn {
		r.Reason = "codex is not logged in; run `codex login` to use your subscription"
		return r, nil //nolint:nilerr // not-logged-in is reported on Readiness, not a Go error
	}
	r.LoggedIn = true
	return r, nil
}

// Command builds the codex exec invocation for one episode: read-only sandbox, native
// approvals denied, the MCP client pointed at the bridge over streamable HTTP with the
// token in the environment, and the final message read from a file. The turn goes on
// stdin, with the standing instruction as a lower-authority preamble.
func (c *Codex) Command(ep Episode) (Invocation, error) {
	bridge := ep.Bridge
	if bridge.Name == "" {
		bridge.Name = codexBridgeName
	}
	lastMsg := "codex-last-message.txt"

	// codex resumes the thread it announced on an earlier episode of this run, and
	// every exec flag below still applies to the resume subcommand.
	args := []string{"exec"}
	if ep.Session != "" {
		args = append(args, "resume", ep.Session)
	}
	args = append(args,
		"--json",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--output-last-message", lastMsg,
		// With a read-only sandbox and no approval path codex cannot escalate to a
		// native write or command.
		"-c", "approval_policy=\"never\"",
		// The rmcp client is codex's only HTTP MCP client; the legacy client speaks
		// stdio only, so without it the child never sends the token and has no governed
		// tools. Recent builds accept this top-level key as a compatibility alias.
		"-c", "experimental_use_rmcp_client=true",
		// Disable the two built-ins codex would run unobserved: web search (egress
		// outside the gate) and the image viewer. Shell and patch cannot be disabled and
		// are contained by the sandbox; native reads stay possible on a read-only host,
		// which the capability notice steers toward the bridge.
		"-c", "tools.web_search=false",
		"-c", "tools.view_image=false",
		// The token is read from the environment, out of the process table.
		"-c", "mcp_servers."+bridge.Name+".url=\""+bridge.URL+"\"",
		"-c", "mcp_servers."+bridge.Name+".bearer_token_env_var=\""+bridge.TokenEnv+"\"",
	)
	if ep.Workdir != "" {
		args = append(args, "-C", ep.Workdir)
	}
	if ep.Model != "" {
		args = append(args, "-m", ep.Model)
	}
	// "-" reads the turn from stdin, so a long or multi-line input stays off the
	// command line.
	args = append(args, "-")

	// The notice leads the preamble so the model reads the tool contract before the
	// objective.
	system := codexCapabilityNotice
	if ep.System != "" {
		system += "\n\n" + ep.System
	}
	stdin := promptLayers{system: system, probes: ep.Probes, input: ep.Input}.render()

	inv := Invocation{
		Path:            c.bin,
		Args:            args,
		Stdin:           stdin,
		LastMessageFile: lastMsg,
	}
	if bridge.TokenEnv != "" && bridge.Token != "" {
		inv.Env = append(inv.Env, bridge.TokenEnv+"="+bridge.Token)
	}
	return inv, nil
}

// codexEvent is one line of codex exec --json, decoded only as far as the projection
// needs.
type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// ThreadID is the conversation opened or resumed, announced on thread.started; it is
	// what `exec resume` takes.
	ThreadID string          `json:"thread_id"`
	Error    *codexErr       `json:"error"`
	Usage    *codexUsage     `json:"usage"`
	Item     *codexItem      `json:"item"`
	Raw      json.RawMessage `json:"-"`
}

type codexErr struct {
	Message string `json:"message"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// codexItem is a thread item. codex flattens the typed payload into the item, so Type
// selects the populated fields.
type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// mcp_tool_call
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`

	// command_execution
	Command string `json:"command"`

	// Status is in_progress, completed, failed, or declined for a command codex refused.
	Status string `json:"status"`
}

// Parse projects one codex exec --json line to events. Thread and turn starts are
// attested progress; an error or failed turn is an error, terminal when the message
// names a permanent condition; a completed turn is done with any usage; an assistant
// message is attested text. An undecodable line is attested progress with the raw line.
func (c *Codex) Parse(line []byte) ([]Event, error) {
	line = trimLine(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// A non-JSON line is CLI noise (a banner, a warning).
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: cloneRaw(line)}}, nil //nolint:nilerr // noise is recorded, not fatal
	}
	raw := cloneRaw(line)
	var out []Event
	switch ev.Type {
	case "turn.completed":
		e := Event{Kind: EventDone, Tier: TierAttested, Raw: raw}
		if ev.Usage != nil {
			e.Usage = Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
		}
		out = []Event{e}
	case "error":
		out = []Event{{Kind: EventError, Err: ev.Message, Terminal: terminalCodexError(ev.Message), Tier: TierAttested, Raw: raw}}
	case "turn.failed":
		msg := ""
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		out = []Event{{Kind: EventError, Err: msg, Terminal: terminalCodexError(msg), Tier: TierAttested, Raw: raw}}
	case "item.completed":
		// codex also emits item.started and item.updated for the same call; projecting
		// those would count one call up to three times and skew the steering metrics.
		out = []Event{c.projectItem(ev.Item, raw)}
	case "item.started", "item.updated":
		// Assistant text is projected on item.started so the live trace streams it.
		if ev.Item != nil && isAgentMessage(ev.Item.Type) && ev.Item.Text != "" && ev.Type == "item.started" {
			out = []Event{{Kind: EventText, Text: ev.Item.Text, Tier: TierAttested, Raw: raw}}
		} else {
			out = []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
		}
	default:
		out = []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}
	}
	// A later episode needs the thread id to continue this thread.
	return withSession(out, ev.ThreadID), nil
}

// projectItem projects a completed item. Bridge calls and native commands are told
// apart because tuning needs to know which tools the harness chose; everything else is
// attested progress.
func (c *Codex) projectItem(item *codexItem, raw json.RawMessage) Event {
	if item == nil {
		return Event{Kind: EventProgress, Tier: TierAttested, Raw: raw}
	}
	switch item.Type {
	case "agent_message", "assistant_message":
		if item.Text != "" {
			return Event{Kind: EventText, Text: item.Text, Tier: TierAttested, Raw: raw}
		}
	case "mcp_tool_call":
		return Event{
			Kind: EventBridgeCall, Tier: TierAttested, Raw: raw,
			Server: item.Server, Tool: item.Tool, Args: item.Arguments, Status: item.Status,
		}
	case "command_execution", "file_change":
		// A native shell or patch tool. The sandbox stops a write, but a read succeeds
		// and the run never sees what it read.
		return Event{
			Kind: EventNativeCommand, Tier: TierAttested, Raw: raw,
			Command: item.Command, Status: item.Status,
		}
	}
	return Event{Kind: EventProgress, Tier: TierAttested, Raw: raw}
}

// isAgentMessage reports whether an item is the assistant's visible message:
// agent_message, or assistant_message from an older build.
func isAgentMessage(t string) bool {
	return t == "agent_message" || t == "assistant_message"
}

// terminalCodexError reports whether an error is permanent (auth, an unsupported model,
// exhausted quota), so the run stops instead of retrying. Unrecognized errors are
// transient.
func terminalCodexError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"not supported", "unauthorized", "invalid_api_key", "invalid api key", "insufficient_quota", "quota", "forbidden", "401", "403"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// trimLine drops surrounding whitespace, including a CRLF line's carriage return.
func trimLine(line []byte) []byte {
	return []byte(strings.TrimSpace(string(line)))
}

// cloneRaw copies a line so Raw does not alias a reused scan buffer.
func cloneRaw(line []byte) json.RawMessage {
	out := make([]byte, len(line))
	copy(out, line)
	return out
}

var _ Adapter = (*Codex)(nil)
