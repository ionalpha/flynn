package externagent

import (
	"context"
	"encoding/json"
	"strings"
)

// Codex drives the codex CLI as an external agent backend over its non-interactive
// exec mode, which streams an episode as JSON lines and writes the final message to a
// file. It is constrained to a read-only native sandbox with native approvals denied,
// so the model cannot write or run a command directly; its effects reach the
// workspace only through the loopback MCP bridge, where they are governed.
type Codex struct {
	// bin is the codex executable name or path; empty uses "codex" on PATH.
	bin string
	// spawner runs the detection probes. It may be nil for an adapter used only to
	// build invocations and parse output (the runner supplies its own spawner for the
	// episode itself); Detect requires it.
	spawner Spawner
}

// NewCodex builds the codex adapter. bin overrides the executable to run (empty uses
// "codex" resolved on PATH); spawner runs the detection probes through the sandbox
// boundary and may be nil when the adapter is used only for Command and Parse.
func NewCodex(bin string, spawner Spawner) *Codex {
	if bin == "" {
		bin = "codex"
	}
	return &Codex{bin: bin, spawner: spawner}
}

// Name identifies the adapter in the model spec and on the run.
func (*Codex) Name() string { return "codex" }

// codexBridgeName is the MCP server name the bridge is registered under in codex's
// config. Tool names codex reports are namespaced by it.
const codexBridgeName = "flynn"

// Detect probes that codex is installed, logged in, and new enough to be constrained
// to the bridge. It runs codex's own version, auth, and help probes and never starts
// an episode. A missing binary or a build without the lockdown knobs (the read-only
// sandbox, the JSON event stream, the streamable-HTTP MCP client) is a hard refusal,
// so the driver stops rather than running codex with unattested effects; a healthy
// but logged-out CLI is a recoverable onboarding prompt.
func (c *Codex) Detect(ctx context.Context) (Readiness, error) {
	if c.spawner == nil {
		return Readiness{Refuse: true, Reason: "codex detection needs a process spawner"}, nil
	}
	version, err := c.spawner.Probe(ctx, c.bin, "--version")
	if err != nil {
		// A failed probe is unreadiness, not a Go error: report an actionable reason so
		// the caller can onboard rather than crash.
		return Readiness{Reason: "codex CLI not found on PATH; install it to use the codex backend"}, nil //nolint:nilerr // probe failure is reported as unreadiness
	}
	r := Readiness{Available: true, Version: strings.TrimSpace(version)}

	// The lockdown depends on three knobs: the read-only sandbox and the JSON event
	// stream on exec, and a streamable-HTTP MCP client. A build missing any of them
	// cannot be constrained to route effects through the bridge, so refuse rather than
	// run with native effects.
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

	// Login state: codex login status exits zero and reports a logged-in line when
	// credentials are usable. A logged-out CLI is an onboarding prompt, not a refusal.
	status, err := c.spawner.Probe(ctx, c.bin, "login", "status")
	if err != nil || !strings.Contains(strings.ToLower(status), "logged in") {
		r.Reason = "codex is not logged in; run `codex login` to use your subscription"
		return r, nil //nolint:nilerr // not-logged-in is reported on Readiness, not a Go error
	}
	r.LoggedIn = true
	return r, nil
}

// Command builds the codex exec invocation for one episode. It pins the read-only
// sandbox and denies native approvals so codex cannot write or run a command itself,
// points codex's MCP client at the bridge over streamable HTTP with the bearer token
// carried in the environment (not on the command line), and reads the final message
// from a file rather than the event stream. The turn is written on stdin; a standing
// instruction is prepended as a lower-authority preamble, since codex's own harness
// prompt outranks anything injected.
func (c *Codex) Command(ep Episode) (Invocation, error) {
	bridge := ep.Bridge
	if bridge.Name == "" {
		bridge.Name = codexBridgeName
	}
	lastMsg := "codex-last-message.txt"

	args := []string{
		"exec",
		"--json",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--output-last-message", lastMsg,
		// Deny native approvals: with a read-only sandbox and no approval path, codex
		// cannot escalate to a native write or command; effects must go through the bridge.
		"-c", "approval_policy=\"never\"",
		// Point codex's MCP client at the loopback bridge over streamable HTTP, with the
		// bearer token read from the environment so it is not in the process table.
		"-c", "mcp_servers." + bridge.Name + ".url=\"" + bridge.URL + "\"",
		"-c", "mcp_servers." + bridge.Name + ".bearer_token_env_var=\"" + bridge.TokenEnv + "\"",
	}
	if ep.Workdir != "" {
		args = append(args, "-C", ep.Workdir)
	}
	if ep.Model != "" {
		args = append(args, "-m", ep.Model)
	}
	// The turn is read from stdin (the "-" prompt), so a long or multi-line input does
	// not land on the command line.
	args = append(args, "-")

	stdin := ep.Input
	if ep.System != "" {
		stdin = ep.System + "\n\n" + ep.Input
	}

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

// codexEvent is the envelope codex exec --json emits, one JSON object per line. Only
// the fields the projection needs are decoded; the rest is preserved in Raw.
type codexEvent struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Error   *codexErr       `json:"error"`
	Usage   *codexUsage     `json:"usage"`
	Item    *codexItem      `json:"item"`
	Raw     json.RawMessage `json:"-"`
}

type codexErr struct {
	Message string `json:"message"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Parse projects one codex exec --json line to typed events. Recognized boundaries
// (thread and turn start) become attested progress; a reported error or a failed
// turn becomes an error event, terminal when the message names a permanent
// condition; a completed turn becomes a done event carrying any usage; an assistant
// message item becomes attested text. A line it cannot decode becomes an attested
// progress event carrying the raw line, so nothing is dropped and noise does not end
// the episode.
func (c *Codex) Parse(line []byte) ([]Event, error) {
	line = trimLine(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// A line that is not JSON is CLI noise (a banner, a warning), kept as attested
		// progress rather than dropped or treated as a fatal parse error.
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: cloneRaw(line)}}, nil //nolint:nilerr // noise is recorded, not fatal
	}
	raw := cloneRaw(line)
	switch ev.Type {
	case "turn.completed":
		e := Event{Kind: EventDone, Tier: TierAttested, Raw: raw}
		if ev.Usage != nil {
			e.Usage = Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
		}
		return []Event{e}, nil
	case "error":
		return []Event{{Kind: EventError, Err: ev.Message, Terminal: terminalCodexError(ev.Message), Tier: TierAttested, Raw: raw}}, nil
	case "turn.failed":
		msg := ""
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		return []Event{{Kind: EventError, Err: msg, Terminal: terminalCodexError(msg), Tier: TierAttested, Raw: raw}}, nil
	case "item.completed", "item.started":
		if ev.Item != nil && (ev.Item.Type == "agent_message" || ev.Item.Type == "assistant_message") && ev.Item.Text != "" {
			return []Event{{Kind: EventText, Text: ev.Item.Text, Tier: TierAttested, Raw: raw}}, nil
		}
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}, nil
	default:
		return []Event{{Kind: EventProgress, Tier: TierAttested, Raw: raw}}, nil
	}
}

// terminalCodexError reports whether an error message names a permanent condition
// (auth, an unsupported model, exhausted quota) that a retry cannot fix, so the run
// stops instead of looping. An unrecognized error is treated as transient.
func terminalCodexError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"not supported", "unauthorized", "invalid_api_key", "invalid api key", "insufficient_quota", "quota", "forbidden", "401", "403"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// trimLine drops a trailing carriage return and surrounding whitespace, so a line
// framed with CRLF parses cleanly.
func trimLine(line []byte) []byte {
	return []byte(strings.TrimSpace(string(line)))
}

// cloneRaw copies a line so the retained Raw does not alias a reused scan buffer.
func cloneRaw(line []byte) json.RawMessage {
	out := make([]byte, len(line))
	copy(out, line)
	return out
}

var _ Adapter = (*Codex)(nil)
