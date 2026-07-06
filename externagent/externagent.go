// Package externagent runs another agent's command-line harness as a backend, so a
// run can be driven by an external agent CLI (installed on the host, authenticated
// on its own subscription) instead of a single model call. The external tool owns
// its own inner loop: handed one turn, it runs a whole agentic episode (many inner
// model calls, its own tool calls and retries) before it yields. That is the
// opposite of a model port, which is one request in and one response out, so an
// external agent is a loop strategy, not a model.
//
// The external tool must never be the effector. Its native execution surface is
// locked down (a read-only sandbox, native writes and shell denied), and the
// agent's own tools are offered to it through a loopback Model Context Protocol
// bridge the run hosts. Every tool call the external agent makes comes back through
// that bridge and is admitted, contained, braked, and recorded at the same dispatch
// waist as a native loop. So swapping in another harness never widens what a run may
// do or escapes a halt; the waist is outside the loop and applies to every action,
// whichever harness took it.
//
// What an external harness cannot make observable is recorded as a gap rather than
// hidden: its inner model calls, the context it compacts, and its direct channel to
// its own provider are outside the run's tracing. Each projected action carries a
// provenance Tier saying how strongly the record can vouch for it, so a run driven
// by an external agent never claims the integrity of a native run.
//
// One Adapter describes one external CLI; the codex adapter is the first. The runner,
// the bridge, and the governance are shared, so a second CLI is a new Adapter, not a
// new subsystem.
package externagent

import (
	"context"
	"encoding/json"
	"io"
)

// Spawner runs the external CLI's processes. Process spawning is confined to the
// sandbox boundary in this project, so this package never spawns directly: the
// caller provides a Spawner backed by that boundary (a semi-trusted external-agent
// containment profile), and the adapter and runner drive it. Probe runs a short
// command to completion for detection; Start launches an episode subprocess bound to
// the context, so cancelling the context kills it.
type Spawner interface {
	// Probe runs path with args to completion and returns its combined output. A
	// non-nil error means the command could not be run or exited non-zero, which
	// detection reads as "not present" or "not ready".
	Probe(ctx context.Context, path string, args ...string) (string, error)
	// Start launches one episode's subprocess and returns its live stdout and a wait
	// handle. The process is bound to ctx: a cancellation (a halt or shutdown) kills it.
	Start(ctx context.Context, ep Episode, inv Invocation) (Process, error)
}

// Process is a running episode subprocess: its stdout stream and a wait handle.
// Cancellation is the context's job (Start binds the process to it), so there is no
// separate kill; Wait returns once the process ends, including after a
// context-driven kill.
type Process interface {
	Stdout() io.Reader
	Wait() error
}

// Readiness reports whether an external agent CLI can start an episode, and if not,
// why. The driver consults it before every session: a CLI that is not installed or
// not logged in yields an actionable Reason, and a CLI that cannot be constrained to
// route its effects through the bridge (too old to offer the sandbox, approval, or
// MCP knobs the lockdown needs) sets Refuse, so the driver stops rather than running
// an external harness with unattested effects.
type Readiness struct {
	// Available is true when the CLI is installed and answered a version probe.
	Available bool
	// LoggedIn is true when the CLI holds usable credentials (a subscription session
	// or an API key), so an episode would not stall on an auth prompt.
	LoggedIn bool
	// Version is the CLI's reported version, recorded on the run and used to refuse a
	// build too old to constrain.
	Version string
	// Reason is a one-line, actionable message when the CLI is not ready to run (for
	// example, an instruction to log in). It is empty when Ready is true.
	Reason string
	// Refuse marks a hard refusal the driver must not start on (a too-old CLI, a
	// missing lockdown knob) as distinct from a recoverable onboarding prompt (not yet
	// logged in). A refusal is terminal; an onboarding prompt is not.
	Refuse bool
}

// Ready reports whether an episode can start: the CLI is installed, logged in, and
// not under a hard refusal.
func (r Readiness) Ready() bool { return r.Available && r.LoggedIn && !r.Refuse }

// Bridge is the loopback endpoint an episode points the external CLI at, so the
// CLI's tool calls come back through the dispatch waist. It is a streamable-HTTP MCP
// server the run hosts on the local host for the life of the episode.
type Bridge struct {
	// Name is the MCP server name the CLI registers the bridge under. Tool names the
	// CLI reports may be namespaced by it, which the driver maps back to real tool
	// identities for capability matching.
	Name string
	// URL is the streamable-HTTP endpoint the CLI connects to, on the loopback
	// interface.
	URL string
	// Token is the bearer token the CLI must present, so another local process cannot
	// drive the bridge. The adapter passes it to the CLI through an environment
	// variable rather than an argument, keeping it out of the process table.
	Token string
	// TokenEnv is the environment variable the CLI reads the bearer token from.
	TokenEnv string
}

// Episode is one turn handed to an external CLI: the user input to act on, where to
// act, which model to drive, the standing instruction to layer in, and the bridge to
// route effects through. The system instruction lands as a lower-authority layer
// because the CLI's own harness prompt outranks anything injected, so a behavioral
// contract is a request to an external harness, not a guarantee.
type Episode struct {
	Input   string
	Workdir string
	Model   string
	System  string
	Bridge  Bridge
}

// Invocation is the subprocess to run for one episode: the program to exec, its
// arguments, environment additions (KEY=VALUE, merged onto a minimal base by the
// runner), and the input to write on stdin when the CLI reads the turn from stdin
// rather than an argument. LastMessageFile, when set, is the path the CLI writes its
// final assistant message to, which the runner reads as the episode's result rather
// than reconstructing it from the event stream.
type Invocation struct {
	Path            string
	Args            []string
	Env             []string
	Stdin           string
	LastMessageFile string
}

// Tier is the provenance tier of a recorded action: how strongly the sealed record
// can vouch for it. A run driven by an external harness mixes tiers, and verify
// reports the mix so an external-harness run never claims the integrity of a native
// one.
type Tier int

const (
	// TierEnforced is an action that ran through the dispatch waist: a bridged tool
	// call, admitted against the grant and contained, exactly like a native action.
	TierEnforced Tier = iota
	// TierAttested is an action the external CLI reported that the run did not
	// independently enforce, for example the CLI's own progress and context events.
	// The record carries the CLI's claim, marked as its claim.
	TierAttested
	// TierUnobserved is a declared gap: work the external harness does that is
	// structurally outside the run's tracing (its inner model calls, its direct egress
	// to its own provider). The record names the gap rather than pretending to cover it.
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

// EventKind is the sort of thing an episode's output line projects to.
type EventKind int

const (
	// EventProgress is an intermediate signal from the CLI with no effect of its own
	// (a thread or turn boundary, a status line). It is attested.
	EventProgress EventKind = iota
	// EventText is assistant-visible text the CLI produced. It is attested: the run
	// did not generate it and cannot attest the context that produced it.
	EventText
	// EventUsage carries token accounting the CLI reported for the episode.
	EventUsage
	// EventError is a failure the CLI reported (a provider error, a turn failure). Its
	// Class distinguishes a terminal failure from a transient one for retry.
	EventError
	// EventDone marks the episode's own completion (the turn finished), carrying any
	// final usage. It is distinct from EventError, which ends an episode abnormally.
	EventDone
)

// Event is one typed projection of an episode's output line. The runner forwards it
// to the reporter and, with its Tier, to the record. Raw preserves the original CLI
// line for the attested record, so the CLI's own account is kept verbatim alongside
// the typed projection.
type Event struct {
	Kind     EventKind
	Text     string
	Usage    Usage
	Err      string
	Terminal bool // for EventError: the failure is terminal, not worth retrying
	Tier     Tier
	Raw      json.RawMessage
}

// Usage is the token accounting an episode reports. It mirrors the shape a native
// run records, so an external episode's cost lands on the same accounting.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Adapter describes how to drive one external agent CLI as a subprocess. The codex
// adapter is the first; the same port fits any CLI that runs an episode as a child
// process and reports it as a stream of lines. An implementation must be safe for
// concurrent use.
type Adapter interface {
	// Name is the CLI's stable identifier (for example "codex"), used in the model
	// spec that selects it and recorded on the run.
	Name() string
	// Detect probes whether the CLI is installed, logged in, and new enough to be
	// constrained. It runs the CLI's own version and auth probes and never starts an
	// episode.
	Detect(ctx context.Context) (Readiness, error)
	// Command builds the subprocess invocation for one episode: the argv that locks
	// the CLI's native execution down (read-only, native effects denied) and points
	// its MCP client at the bridge, plus how the turn and the final message are
	// carried. It does not run anything.
	Command(ep Episode) (Invocation, error)
	// Parse turns one line of the CLI's stdout into zero or more typed events. A line
	// it does not recognize yields an attested progress event rather than an error, so
	// an unfamiliar line is recorded, not dropped.
	Parse(line []byte) ([]Event, error)
}
