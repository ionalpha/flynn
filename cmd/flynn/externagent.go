package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/sandbox"
)

// externalAgentBackends are the CLI harness names selectable through the --model
// spec. A spec whose scheme (the part before the first colon) names one of these is
// not a hosted model to resolve a credential for: it routes the run to that external
// agent CLI driver, and the rest of the spec is the model the CLI itself should
// drive. Adding a second harness is one entry here plus a case in newExternalAgent,
// not a new resolution path.
var externalAgentBackends = map[string]bool{"codex": true, "claude": true}

// externalAgentSpec splits a --model spec into an external-agent backend name and the
// model string to hand the CLI, reporting whether the spec names an external agent at
// all. A bare name selects the CLI with its own default model; a name:model spec pins
// the model. "codex" -> ("codex", "", true); "codex:gpt-5-codex" -> ("codex",
// "gpt-5-codex", true); "anthropic:claude-..." -> ("", "", false).
func externalAgentSpec(modelSpec string) (name, model string, ok bool) {
	name, model, _ = strings.Cut(modelSpec, ":")
	if !externalAgentBackends[name] {
		return "", "", false
	}
	return name, model, true
}

// externalAgentNames lists the backends in a stable order, for messages that name
// what is available.
func externalAgentNames() []string {
	names := make([]string, 0, len(externalAgentBackends))
	for n := range externalAgentBackends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// errExternalAgentUnsupported reports that command cannot drive modelSpec because the
// spec names an external agent, and points at the invocations that can. A long-lived
// server answers many independent requests, and an external agent's episode is one
// conversation the CLI holds; nothing decides which request continues which
// conversation, so the combination is refused rather than guessed at. Without this the
// spec falls through to the native provider resolver, which has never heard of the
// backend and rejects it as an unknown provider: true, but no help at all.
//
// It returns nil when the spec names a model, so a normal run is never blocked.
func errExternalAgentUnsupported(command, modelSpec string) error {
	name, _, ok := externalAgentSpec(modelSpec)
	if !ok {
		return nil
	}
	return fmt.Errorf("%s cannot drive the %s external agent backend: it brings its own harness and holds its own conversation, "+
		"which has no meaning across a server's independent requests. "+
		"Drive it from a session, `flynn --model %s`, a goal, `flynn --model %s goal \"<objective>\"`, "+
		"or a review, `flynn review <owner/repo#n> --model %s`. "+
		"To run %s itself, name a model provider (see `flynn models`)",
		command, name, modelSpec, modelSpec, modelSpec, command)
}

// externAgent is a resolved external-agent backend: the driver that builds the
// episode loop and the model string the CLI drives, ready to plug into a run in place
// of a native llm.Model.
type externAgent struct {
	model  string
	driver driver.Driver
}

// close releases what the run's episodes shared, the harness's credential-and-state home,
// once the run that resolved this agent is over. A driver that holds nothing closes to
// nothing, so every caller can defer it unconditionally.
func (ea *externAgent) close() {
	if ea == nil {
		return
	}
	if c, ok := ea.driver.(io.Closer); ok {
		_ = c.Close()
	}
}

// tierTallier is an external-agent driver that tallies the provenance tiers of the
// events its episodes projected, how the harness chose its tools, and which parts of the
// session contract it ignored. The driver port itself stays free of provenance (a native
// loop has no tiers to report and no contract to drift from), so the host asks for the
// capability rather than requiring it.
type tierTallier interface {
	Tiers() map[externagent.Tier]int
	Steering() externagent.Steering
	Drift() map[string]int
}

// observedProvenance is what the host can say about an external-harness run once it has
// finished: the harness's own account of itself, and how far it strayed from the contract
// the run gave it. A driver that reports none of this yields a bare declaration, which
// still names the run as externally driven.
func observedProvenance(ea *externAgent) externalProvenance {
	d := externalProvenance{harness: ea.driver.Name()}
	t, ok := ea.driver.(tierTallier)
	if !ok {
		return d
	}
	d.attested = t.Tiers()[externagent.TierAttested]
	d.nativeRate = t.Steering().NativeRate()
	d.drift = t.Drift()
	return d
}

// codexAllowedHosts are the destination names a codex episode's confined child may
// reach at the egress waist: the OpenAI API and ChatGPT subscription-auth endpoints
// codex talks to. Everything else is denied, so the external harness's own provider
// channel is contained to these names even though its inner traffic is unobserved. A
// name that resolves to a private or rebinding address is still refused by the address
// gate.
var codexAllowedHosts = []string{
	"api.openai.com",
	"auth.openai.com",
	"chatgpt.com",
	".chatgpt.com",
}

// codexAuthDir is the codex CLI's credential and config home, granted read to the
// confined child so an auth-status probe and a live episode can see the subscription
// token without it being copied into the workspace. CODEX_HOME overrides the default
// (~/.codex), matching the CLI's own resolution. Empty when no home directory is
// resolvable, which grants no extra read.
func codexAuthDir() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexAuthSeedFiles are the only files an episode copies out of the codex home into the
// per-episode credential home the confined child writes: the OAuth token it authenticates
// with, and nothing else. Everything else in the home (session rollouts, logs, caches) is
// state the CLI regenerates, so it is left behind rather than handed to an untrusted
// harness. See externagent.SandboxConfig.AuthSeedFiles for why an episode gets a copy
// instead of the real directory.
//
// The operator's own config.toml is deliberately not among them, which is the codex half
// of the HostConfig posture the adapter declares. That file is where a codex user keeps
// their MCP servers, model providers and settings, so seeding it would let whatever the
// person who launched the run happens to carry steer the episode, and would give the
// harness servers this run neither hosts nor governs. Everything an episode needs is
// passed on the command line instead, so the child's configuration is a function of the
// run.
var codexAuthSeedFiles = []string{"auth.json"}

// claudeAllowedHosts are the destination names a claude episode's confined child may
// reach at the egress waist: the Anthropic API the subscription drives inference over and
// the endpoints its OAuth session refreshes against. Everything else is denied, so the
// external harness's own provider channel is contained to these names even though its
// inner traffic is unobserved. A name that resolves to a private or rebinding address is
// still refused by the address gate. Telemetry and update hosts are deliberately left off
// until a real run shows one is needed to authenticate or infer.
var claudeAllowedHosts = []string{
	"api.anthropic.com",
	"console.anthropic.com",
	"claude.ai",
	".claude.ai",
}

// claudeHome is the Claude Code home Flynn keeps for the claude backend, apart from the
// user's own. The user signs the CLI in to it once with the CLI's own login
// ([claudeLoginCommand]), and every probe and episode points the CLI at it through
// CLAUDE_CONFIG_DIR. Flynn never reads, copies or stores the token inside: the CLI writes it
// at sign-in and refreshes it in place, and the sign-in completes in the CLI's own flow.
//
// Keeping it apart from the user's home is what makes it safe to grant an episode write on
// it ([externagent.SandboxConfig.AuthDirWritable]): the worst a harness can do there is sign
// Flynn's copy of the CLI out. It also means none of the user's own settings, hooks,
// plugins or MCP servers are in it to steer an episode. It sits under the per-user data
// directory, so a development build and a release build keep separate sign-ins. A variable
// so tests can point it at a temporary directory.
var claudeHome = defaultClaudeHome

// defaultClaudeHome is where [claudeHome] points outside tests.
func defaultClaudeHome() string { return filepath.Join(defaultDataDir(), "claude") }

// claudeLoginCommand is the command that signs the claude CLI in to home with its own
// subscription login, written for the shell the user is most likely in. The PowerShell form
// clears the variable afterwards, so the user's next plain `claude` in that shell is back on
// their own home.
func claudeLoginCommand(home string) string {
	if runtime.GOOS == "windows" {
		return "$env:CLAUDE_CONFIG_DIR = '" + strings.ReplaceAll(home, "'", "''") +
			"'; claude auth login --claudeai; Remove-Item Env:CLAUDE_CONFIG_DIR"
	}
	return "CLAUDE_CONFIG_DIR='" + strings.ReplaceAll(home, "'", `'\''`) + "' claude auth login --claudeai"
}

// withClaudeLogin rewrites a not-signed-in readiness for the claude backend so its next step
// signs in Flynn's own Claude Code home rather than the user's: the adapter's generic advice
// (`claude auth login`) would sign in the home Flynn does not use and leave the backend
// reporting itself logged out. A refusal, a missing install, and a ready CLI pass through.
func withClaudeLogin(r externagent.Readiness, home string) externagent.Readiness {
	if !r.Available || r.Refuse || r.LoggedIn {
		return r
	}
	r.Reason = "claude is not signed in to a Claude subscription for Flynn. Flynn keeps its own Claude Code sign-in, separate from yours, at " +
		home + ". Sign in to it once with Claude Code's own login: " + claudeLoginCommand(home)
	return r
}

// externalProbeTimeout caps a single detection probe (a version or auth-status check)
// so a hung CLI cannot stall onboarding.
const externalProbeTimeout = 15 * time.Second

// externalSpawner builds the production sandbox spawner for the named backend: the
// external CLI runs as an untrusted subprocess under the kernel-confined floor, with a
// deny-all-except-provider egress gate and a read grant for its own auth home. One
// place builds the confinement envelope so detection and the live episode run under
// the same boundary and provider allowlist, and cannot drift.
func externalSpawner(name string) (externagent.Spawner, error) {
	switch name {
	case "codex":
		// The CLI is resolved before the envelope is built: the confined child is granted
		// read on the directories holding the executable, so it must be located first. A
		// CLI that is not installed still yields a spawner, so detection can report the
		// missing install as onboarding rather than failing the command outright.
		prog, err := externagent.LocateCodex("")
		if err != nil && !errors.Is(err, externagent.ErrProgramNotFound) {
			return nil, err
		}
		return externagent.NewSandboxSpawner(externagent.SandboxConfig{
			AllowedHosts:  codexAllowedHosts,
			AuthDir:       codexAuthDir(),
			AuthEnv:       "CODEX_HOME",
			AuthSeedFiles: codexAuthSeedFiles,
			ProgramDirs:   prog.ReadableDirs,
			ProbeTimeout:  externalProbeTimeout,
			// The codex CLI is a Rust program: it canonicalizes paths on startup, which no
			// process can do inside a Windows AppContainer, so it runs under the tier that
			// confines its writes to the workspace and leaves the host readable. Its writes,
			// its egress, and its tools stay gated; only the read posture is traded, and the
			// episode records the tier that ran.
			HostReadable: true,
		}), nil
	case "claude":
		prog, err := externagent.LocateClaude("")
		if err != nil && !errors.Is(err, externagent.ErrProgramNotFound) {
			return nil, err
		}
		// The CLI would create its home at sign-in, but detection grants the child read on it
		// before then, and a grant on a directory that does not exist fails the probe that
		// reports how to sign in.
		home := claudeHome()
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("claude home: %w", err)
		}
		return externagent.NewSandboxSpawner(externagent.SandboxConfig{
			AllowedHosts:    claudeAllowedHosts,
			AuthDir:         home,
			AuthEnv:         "CLAUDE_CONFIG_DIR",
			AuthDirWritable: true,
			ProgramDirs:     prog.ReadableDirs,
			ProbeTimeout:    externalProbeTimeout,
			// Claude Code ships as a single compiled native binary that canonicalizes paths on
			// startup, the same operation no process can perform inside a Windows AppContainer,
			// so it runs under the tier that confines its writes to the workspace and leaves the
			// host readable. Its writes, its egress, and its tools stay gated; only the read
			// posture is traded, and the episode records the tier that ran.
			HostReadable: true,
		}), nil
	default:
		return nil, fmt.Errorf("unknown external agent backend %q", name)
	}
}

// externalAdapter builds the CLI adapter for the named backend over spawner. The
// adapter describes how to detect the CLI and drive one episode; the codex adapter is
// the first.
func externalAdapter(name string, spawner externagent.Spawner) (externagent.Adapter, error) {
	switch name {
	case "codex":
		// Launch the native executable the npm launcher stands for, not the launcher: a
		// script would need its interpreter reachable from inside the confinement, and an
		// interpreter under a system-owned directory cannot be granted by an unprivileged
		// process. An absent CLI leaves the name for Detect to report as not installed.
		prog, err := externagent.LocateCodex("")
		if err != nil {
			if !errors.Is(err, externagent.ErrProgramNotFound) {
				return nil, err
			}
			return externagent.NewCodex("", spawner), nil
		}
		return externagent.NewCodex(prog.Path, spawner), nil
	case "claude":
		// Launch the native binary the npm launcher stands for, not the launcher: a shim
		// would need its shell or interpreter reachable from inside the confinement. An
		// absent CLI leaves the name for Detect to report as not installed.
		prog, err := externagent.LocateClaude("")
		if err != nil {
			if !errors.Is(err, externagent.ErrProgramNotFound) {
				return nil, err
			}
			return externagent.NewClaude("", spawner), nil
		}
		return externagent.NewClaude(prog.Path, spawner), nil
	default:
		return nil, fmt.Errorf("unknown external agent backend %q", name)
	}
}

// newExternalAgent builds the driver for the named external agent backend, with
// workdir as the directory the episode operates in. It shares one spawner between the
// adapter and the driver so probes and episodes run under the same confinement. It
// does not probe the CLI; resolveExternalAgent does that first.
func newExternalAgent(name, workdir string) (*externAgent, error) {
	spawner, err := externalSpawner(name)
	if err != nil {
		return nil, err
	}
	adapter, err := externalAdapter(name, spawner)
	if err != nil {
		return nil, err
	}
	return &externAgent{
		driver: externagent.NewDriver(adapter, spawner, workdir),
	}, nil
}

// resolveExternalAgent detects the external agent CLI and, when it is installed,
// logged in, and new enough to be constrained, returns the driver to run it under. It
// mirrors resolveModelOrOnboard's shape for a native provider: a hard refusal (a CLI
// too old to lock down, a missing containment knob) is terminal, and a recoverable
// onboarding gap (not installed, or not logged in) surfaces the CLI's own actionable
// reason ("run `codex login`", "install it") rather than a raw error, so the user
// knows the next step. No API key is ever asked for: an external agent authenticates
// on its own subscription.
func resolveExternalAgent(ctx context.Context, name, model, workdir string) (*externAgent, error) {
	spawner, err := externalSpawner(name)
	if err != nil {
		return nil, err
	}
	adapter, err := externalAdapter(name, spawner)
	if err != nil {
		return nil, err
	}
	r, err := adapter.Detect(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect %s: %w", name, err)
	}
	if name == "claude" {
		r = withClaudeLogin(r, claudeHome())
	}
	if rerr := readinessError(name, r); rerr != nil {
		return nil, rerr
	}
	// A live episode confines the CLI and forces its egress through the governed proxy. Where
	// that leg is not present, the episode would refuse mid-run rather than run the CLI with
	// its network open. Catch it here, once the CLI is otherwise ready, so the user gets one
	// actionable message pointing at a distribution that can run it instead of a governance
	// refusal partway through a goal.
	if !sandbox.GovernedEgressAvailable() {
		return nil, fmt.Errorf("%s %s is installed and signed in, but this platform has no governed-egress leg, so Flynn will not run a live episode with the CLI's network open. Run Flynn inside a WSL2 distribution to drive %s here",
			name, r.Version, name)
	}

	ea, err := newExternalAgent(name, workdir)
	if err != nil {
		return nil, err
	}
	ea.model = model
	fmt.Fprintf(os.Stderr, "Using %s %s on your subscription. Its tool calls are governed and recorded through the flynn bridge; its inner reasoning is recorded as an unobserved-but-contained gap.\n",
		name, r.Version)
	return ea, nil
}

// readinessError turns a not-ready Readiness into the actionable error to surface to
// the user, or nil when the CLI is ready to run. A hard refusal (Refuse) is terminal:
// the CLI cannot be constrained to route its effects through the bridge, so the run
// must not start it with unattested native effects. An onboarding gap (not installed,
// or not logged in) surfaces the adapter's own actionable Reason ("run `codex login`",
// "install it") rather than a raw error, so the user knows the next step; no API key
// is ever requested, since an external agent authenticates on its own subscription.
func readinessError(name string, r externagent.Readiness) error {
	switch {
	case r.Refuse:
		return fmt.Errorf("%s cannot be governed as a backend: %s", name, r.Reason)
	case !r.Ready():
		return fmt.Errorf("%s", r.Reason)
	default:
		return nil
	}
}
