// Package externagent runs another agent's command-line harness (installed on the
// host, signed in on its own subscription) as a run's loop strategy. Handed one turn,
// the external CLI runs a whole episode of inner model calls, tool calls and retries
// before it yields, so it replaces the loop rather than the model port.
//
// The external tool is never the effector. Its native execution is locked down
// (read-only sandbox, native writes and shell denied) and the agent's own tools reach
// it through a loopback Model Context Protocol bridge the run hosts. Every tool call
// comes back through that bridge and is admitted, contained, braked and recorded at the
// same dispatch waist as a native loop, so a different harness never widens what a run
// may do or escapes a halt.
//
// What a harness cannot make observable is recorded as a gap: its inner model calls,
// the context it compacts, and its direct channel to its provider. Each projected action
// carries a provenance [Tier], so an external run never claims a native run's integrity.
//
// One [Adapter] describes one CLI. Each states how its native surface is removed as a
// [Lockdown], which the runner reads before spawning: a provider whose writes or
// commands cannot be removed, or that would run on the operator's own configuration, is
// refused with a reason, so an unconstrainable harness shows up as a missing integration
// instead of a run that looks governed.
package externagent

import (
	"context"
	"encoding/json"
	"io"
)

// Spawner runs the external CLI's processes. Process spawning is confined to the
// sandbox package, so the caller supplies a Spawner backed by it and this package
// never spawns directly.
type Spawner interface {
	// Probe runs path with args to completion and returns its combined output. An
	// error (cannot run, or non-zero exit) reads as "not present" or "not ready".
	Probe(ctx context.Context, path string, args ...string) (string, error)
	// Start launches one episode's subprocess and returns its stdout and a wait
	// handle. Cancelling ctx kills it.
	Start(ctx context.Context, ep Episode, inv Invocation) (Process, error)
}

// Process is a running episode subprocess. It has no kill: cancelling the context
// passed to Start kills it, and Wait returns once it ends.
type Process interface {
	Stdout() io.Reader
	Wait() error
}

// Readiness reports whether an external CLI can start an episode, and if not, why. The
// driver checks it before every session.
type Readiness struct {
	// Available means the CLI is installed and answered a version probe.
	Available bool
	// LoggedIn means the CLI holds usable credentials, so an episode will not stall on
	// an auth prompt.
	LoggedIn bool
	// Version is the CLI's reported version, recorded on the run and used to refuse a
	// build too old to constrain.
	Version string
	// Reason is a one-line, actionable message when the CLI is not ready (for example,
	// an instruction to log in).
	Reason string
	// Refuse marks a terminal refusal (a CLI too old to offer the sandbox, approval or
	// MCP options the lockdown needs), as opposed to a recoverable prompt such as not
	// being logged in.
	Refuse bool
}

// Ready reports whether an episode can start.
func (r Readiness) Ready() bool { return r.Available && r.LoggedIn && !r.Refuse }

// Bridge is the loopback streamable-HTTP MCP server the run hosts for an episode, so
// the CLI's tool calls come back through the dispatch waist.
type Bridge struct {
	// Name is the MCP server name the CLI registers the bridge under. The CLI may
	// namespace tool names with it; the driver maps them back to real tools.
	Name string
	// URL is the address the child dials. Where the child has its own network
	// namespace it is an in-namespace address forwarded to ForwardTo.
	URL string
	// ForwardTo is the host-loopback address the sandbox forwards URL to, or empty when
	// the child reaches URL directly. The runner sets it from what the spawner reports.
	ForwardTo string
	// Token is the bearer token the CLI must present, so another local process cannot
	// drive the bridge. It goes through an environment variable, never an argument, to
	// stay out of the process table.
	Token string
	// TokenEnv is the variable the CLI reads Token from.
	TokenEnv string
}

// Episode is one turn handed to an external CLI. The system instruction is a
// lower-authority layer because the CLI's own harness prompt outranks anything
// injected, so a behavioral contract is a request, never a guarantee.
type Episode struct {
	Input   string
	Workdir string
	Model   string
	System  string
	Bridge  Bridge
	// Probes are conformance instructions folded into the turn. Compliance is checked
	// against the episode's event stream once it finishes.
	Probes []string
	// Session is a conversation the CLI already holds, from an earlier episode, to
	// continue. Only the CLI holds its own earlier context. Empty starts a new one; an
	// adapter whose CLI cannot continue ignores it.
	Session string
}

// Invocation is the subprocess for one episode. Env entries (KEY=VALUE) are merged
// onto a minimal base. Stdin carries the turn for a CLI that reads it there.
// LastMessageFile, when set, is where the CLI writes its final message, which the runner
// reads as the result.
type Invocation struct {
	Path            string
	Args            []string
	Env             []string
	Stdin           string
	LastMessageFile string
}

// Tier is how strongly the sealed record can vouch for an action. An external run
// mixes tiers and verify reports the mix.
type Tier int

const (
	// TierEnforced is a bridged tool call, admitted and contained at the dispatch waist
	// like a native action.
	TierEnforced Tier = iota
	// TierAttested is something the CLI reported that the run did not enforce (its
	// progress and context events), recorded as the CLI's claim.
	TierAttested
	// TierUnobserved is a declared gap outside the run's tracing: the harness's inner
	// model calls and its direct egress to its provider.
	TierUnobserved
)

// String names the tier for the record and verify output.
func (t Tier) String() string {
	switch t {
	case TierEnforced:
		return "enforced"
	case TierAttested:
		return "attested"
	case TierUnobserved:
		return "unobserved"
	default:
		return "unknown"
	}
}

// EventKind is what an episode's output line projects to.
type EventKind int

const (
	// EventProgress is a CLI signal with no effect (a thread or turn boundary, a
	// status line). Attested.
	EventProgress EventKind = iota
	// EventText is text the CLI produced. Attested, since the run cannot vouch for the
	// context that produced it.
	EventText
	// EventUsage carries the token accounting the CLI reported.
	EventUsage
	// EventError is a failure the CLI reported. Terminal separates a final failure from
	// a retryable one.
	EventError
	// EventDone marks the turn finished normally, with any final usage.
	EventDone
	// EventBridgeCall is the CLI's own account of a bridge tool call. The dispatch
	// waist records the call independently; this is used to measure how the harness
	// chose its tools.
	EventBridgeCall
	// EventNativeCommand reports a command or edit the CLI made with its built-in tools.
	// The read-only lockdown contains its effects but the run did not observe them, so it
	// means the harness is being steered poorly; nothing was breached.
	EventNativeCommand
)

// String names the event kind for the record. Record readers match on these values,
// so they must not change.
func (k EventKind) String() string {
	switch k {
	case EventProgress:
		return "progress"
	case EventText:
		return "text"
	case EventUsage:
		return "usage"
	case EventError:
		return "error"
	case EventDone:
		return "done"
	case EventBridgeCall:
		return "bridge_call"
	case EventNativeCommand:
		return "native_command"
	default:
		return "unknown"
	}
}

// Event is one typed projection of an episode's output line. Raw keeps the CLI's line
// verbatim for the attested record.
type Event struct {
	Kind     EventKind
	Text     string
	Usage    Usage
	Err      string
	Terminal bool // for EventError: the failure is terminal, not worth retrying
	Tier     Tier
	Raw      json.RawMessage

	// Tool and Server are the tool and MCP server a bridge call named. A call to another
	// server is not counted as bridged.
	Tool   string
	Server string
	// Args are the bridge call's arguments, verbatim, so a conformance probe can look
	// for the nonce it asked the harness to echo.
	Args json.RawMessage
	// Command is the command line the CLI reported running natively.
	Command string
	// Status is the CLI's status for the call or command: completed, failed, declined
	// or in_progress. A call is reported started and completed, so count one status to
	// avoid double counting.
	Status string
	// Session is the conversation id the CLI announced (claude's init event, codex's
	// thread.started). Both CLIs accept it back to continue the conversation, so a
	// multi-turn session hands the CLI its own context instead of a replayed transcript.
	Session string
}

// withSession stamps a CLI's announced conversation id onto the events from that line,
// so the runner reads it off the stream and adapters need not touch the Result.
func withSession(evs []Event, id string) []Event {
	if id == "" {
		return evs
	}
	for i := range evs {
		evs[i].Session = id
	}
	return evs
}

// Usage is an episode's token accounting, shaped like a native run's so the cost lands
// on the same accounting.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Recorder persists the harness's own account of its episode: each projected event
// with its tier and original line. The dispatch waist records what the run enforced;
// this records what the harness said it did, so a reader can compare the two. A nil
// Recorder records nothing.
type Recorder interface {
	// Record persists one attested event. An error does not fail the episode, because
	// the effects are still enforced and recorded at the waist.
	Record(ctx context.Context, ev Event) error
}

// Adapter drives one external CLI that runs an episode as a child process and reports
// it as a stream of lines. Implementations must be safe for concurrent use.
type Adapter interface {
	// Name is the CLI's stable identifier ("codex"), used in the model spec and
	// recorded on the run.
	Name() string
	// Detect runs the CLI's version and auth probes, never an episode.
	Detect(ctx context.Context) (Readiness, error)
	// Command builds, without running, the invocation that locks the CLI's native
	// execution down and points its MCP client at the bridge.
	Command(ep Episode) (Invocation, error)
	// Lockdown states what Command's invocation leaves the harness holding. The runner
	// refuses a provider it does not strip, so an adapter that cannot lock its CLI down
	// is refused before any native effect runs.
	Lockdown() Lockdown
	// Parse turns one stdout line into events. An unrecognized line becomes an
	// attested progress event, so it is recorded and never dropped.
	Parse(line []byte) ([]Event, error)
}
