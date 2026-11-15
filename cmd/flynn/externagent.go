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

// externalAgentBackends are the CLI harnesses a --model spec can select. A spec whose
// scheme (before the first colon) names one routes the run to that CLI, with the rest
// as the model the CLI drives, and resolves no credential. A new harness is an entry
// here plus a case in newExternalAgent.
var externalAgentBackends = map[string]bool{"codex": true, "claude": true}

// externalAgentSpec splits a --model spec into a backend name and the model to hand the
// CLI, and reports whether it names an external agent. "codex" -> ("codex", "", true);
// "codex:gpt-5-codex" -> ("codex", "gpt-5-codex", true); "anthropic:claude-..." ->
// ("", "", false).
func externalAgentSpec(modelSpec string) (name, model string, ok bool) {
	name, model, _ = strings.Cut(modelSpec, ":")
	if !externalAgentBackends[name] {
		return "", "", false
	}
	return name, model, true
}

// externalAgentNames lists the backends in a stable order for messages.
func externalAgentNames() []string {
	names := make([]string, 0, len(externalAgentBackends))
	for n := range externalAgentBackends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// errExternalAgentUnsupported refuses command for an external-agent modelSpec and
// names the invocations that can run one. A server answers independent requests, but an
// episode is one conversation the CLI holds, and nothing maps requests to
// conversations. Without this the native resolver rejects the backend as an unknown
// provider, which tells the user nothing. It returns nil for an ordinary model spec.
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

// externAgent is a resolved external-agent backend: the driver that builds the episode
// loop and the model the CLI drives, used in place of a native llm.Model.
type externAgent struct {
	model  string
	driver driver.Driver
}

// close releases the harness home the run's episodes shared. It is safe to defer
// unconditionally.
func (ea *externAgent) close() {
	if ea == nil {
		return
	}
	if c, ok := ea.driver.(io.Closer); ok {
		_ = c.Close()
	}
}

// tierTallier is the optional driver capability that tallies its episodes' provenance
// tiers, how the harness chose tools, and which session-contract parts it ignored. The
// driver port carries no provenance, since a native loop has none to report.
type tierTallier interface {
	Tiers() map[externagent.Tier]int
	Steering() externagent.Steering
	Drift() map[string]int
}

// observedProvenance is the harness's account of a finished run and its drift from the
// run's contract. A driver that reports nothing still yields a declaration naming the
// run as externally driven.
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

// codexAllowedHosts are the only names a codex episode's confined child may reach: the
// OpenAI API and ChatGPT subscription-auth endpoints. This contains the harness's
// unobserved provider traffic. The address gate still refuses a name that resolves to
// a private or rebinding address.
var codexAllowedHosts = []string{
	"api.openai.com",
	"auth.openai.com",
	"chatgpt.com",
	".chatgpt.com",
}

// codexHome is Flynn's own codex home, apart from the user's (~/.codex or CODEX_HOME).
// The user signs in once with [codexLoginCommand], and every probe and episode sets
// CODEX_HOME to it. Flynn never reads, copies or stores the token; the CLI writes and
// refreshes it.
//
// The user's config.toml (MCP servers, providers, settings) is never on the child's
// path, so it cannot steer an episode or add servers this run does not govern.
// [clearOwnHome] removes everything but the sign-in before a spawner is built, since
// Flynn writes nothing else there. It is a variable so tests can redirect it.
var codexHome = defaultCodexHome

func defaultCodexHome() string { return filepath.Join(defaultDataDir(), "codex") }

// codexLoginCommand signs the codex CLI in to home with its subscription login.
func codexLoginCommand(home string) string {
	return homeLoginCommand("CODEX_HOME", home, "codex login")
}

// codexAuthFiles and claudeAuthFiles are the sign-in files [clearOwnHome] keeps. A CLI
// rebuilds everything else, and everything else is what a harness would write to steer a
// later run: config.toml, AGENTS.md, prompts and skills for codex; settings.json (hooks,
// permission rules), CLAUDE.md, agents, commands, plugins and .claude.json (user MCP
// servers, trust decisions) for claude. Claude's token is in .credentials.json, so
// dropping .claude.json costs only an account re-read.
var (
	codexAuthFiles  = map[string]bool{"auth.json": true}
	claudeAuthFiles = map[string]bool{".credentials.json": true}
)

// clearOwnHome removes everything from a Flynn CLI home except keep. An episode's child
// may write its home, so without this a harness could leave settings, instructions or
// plugins that steer every later run. A keyring sign-in is outside the home and kept.
func clearOwnHome(label, home string, keep map[string]bool) error {
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s home: %w", label, err)
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, e.Name())); err != nil {
			return fmt.Errorf("%s home: %w", label, err)
		}
	}
	return nil
}

// ensureOwnHome creates a Flynn CLI home owner-only. The home holds a bearer token and
// MkdirAll keeps an existing directory's mode, so the mode is set explicitly. On Windows
// the per-user data directory's ACL governs access and Chmod only flips read-only.
func ensureOwnHome(label, home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("%s home: %w", label, err)
	}
	//nolint:gosec // G302: 0700 is the owner-only mode for a directory; gosec's 0600 ceiling is for files
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("%s home: %w", label, err)
	}
	return nil
}

// claudeAllowedHosts are the only names a claude episode's confined child may reach:
// the Anthropic API and the OAuth refresh endpoints. This contains the harness's
// unobserved provider traffic. The address gate still refuses a private or rebinding
// address. Telemetry and update hosts stay off until a run needs one to authenticate
// or infer.
var claudeAllowedHosts = []string{
	"api.anthropic.com",
	"console.anthropic.com",
	"claude.ai",
	".claude.ai",
}

// claudeHome is Flynn's own Claude Code home, apart from the user's. The user signs in
// once with [claudeLoginCommand], and every probe and episode sets CLAUDE_CONFIG_DIR to
// it. Flynn never reads, copies or stores the token; the CLI writes and refreshes it.
//
// None of the user's settings, hooks, plugins or MCP servers are in it, which is why an
// episode may write it ([externagent.SandboxConfig.AuthDirWritable]). A harness could
// still sign it out or leave files behind, so [clearOwnHome] removes all but the sign-in
// before a spawner is built. It is under the per-user data directory, so dev and release
// builds sign in separately. It is a variable so tests can redirect it.
var claudeHome = defaultClaudeHome

func defaultClaudeHome() string { return filepath.Join(defaultDataDir(), "claude") }

// claudeLoginCommand signs the claude CLI in to home with its subscription login.
func claudeLoginCommand(home string) string {
	return homeLoginCommand("CLAUDE_CONFIG_DIR", home, "claude auth login --claudeai")
}

// homeLoginCommand runs login with env set to home, for the user's likely shell. The
// PowerShell form clears the variable afterwards, so the user's next plain run uses
// their own home.
func homeLoginCommand(env, home, login string) string {
	if runtime.GOOS == "windows" {
		return "$env:" + env + " = '" + strings.ReplaceAll(home, "'", "''") +
			"'; " + login + "; Remove-Item Env:" + env
	}
	return env + "='" + strings.ReplaceAll(home, "'", `'\''`) + "' " + login
}

// withHomeLogin rewrites a not-signed-in readiness to sign in Flynn's home for that CLI.
// The adapter's generic advice (`claude auth login`, `codex login`) signs in the user's
// home and leaves the backend logged out. Every other readiness passes through.
func withHomeLogin(name string, r externagent.Readiness) externagent.Readiness {
	if !r.Available || r.Refuse || r.LoggedIn {
		return r
	}
	switch name {
	case "claude":
		home := claudeHome()
		r.Reason = "claude is not signed in to a Claude subscription for Flynn. Flynn keeps its own Claude Code sign-in, separate from yours, at " +
			home + ". Sign in to it once with Claude Code's own login: " + claudeLoginCommand(home)
	case "codex":
		home := codexHome()
		r.Reason = "codex is not signed in for Flynn. Flynn keeps its own Codex sign-in, separate from yours, at " +
			home + ". Sign in to it once with Codex's own login: " + codexLoginCommand(home)
	}
	return r
}

// externalProbeTimeout caps one detection probe so a hung CLI cannot stall onboarding.
const externalProbeTimeout = 15 * time.Second

// externalSpawner builds the backend's sandbox spawner: the CLI runs untrusted under the
// kernel-confined floor, with egress denied except to its provider and read access to
// its auth home. Detection and live episodes share it, so their boundary and allowlist
// cannot drift.
func externalSpawner(name string) (externagent.Spawner, error) {
	switch name {
	case "codex":
		// The child is granted read on the executable's directories, so the CLI is located
		// first. A missing CLI still yields a spawner, so detection reports it as onboarding.
		prog, err := externagent.LocateCodex("")
		if err != nil && !errors.Is(err, externagent.ErrProgramNotFound) {
			return nil, err
		}
		// The CLI refuses a missing CODEX_HOME, and the first sign-in uses the printed path.
		home := codexHome()
		if err := ensureOwnHome("codex", home); err != nil {
			return nil, err
		}
		if err := clearOwnHome("codex", home, codexAuthFiles); err != nil {
			return nil, err
		}
		return externagent.NewSandboxSpawner(codexSandboxConfig(home, prog)), nil
	case "claude":
		prog, err := externagent.LocateClaude("")
		if err != nil && !errors.Is(err, externagent.ErrProgramNotFound) {
			return nil, err
		}
		// Detection grants read on the home before sign-in, and a grant on a missing directory
		// fails the probe that explains how to sign in.
		home := claudeHome()
		if err := ensureOwnHome("claude", home); err != nil {
			return nil, err
		}
		if err := clearOwnHome("claude", home, claudeAuthFiles); err != nil {
			return nil, err
		}
		return externagent.NewSandboxSpawner(claudeSandboxConfig(home, prog)), nil
	default:
		return nil, fmt.Errorf("unknown external agent backend %q", name)
	}
}

// codexSandboxConfig lets the CLI write its home
// ([externagent.SandboxConfig.AuthDirWritable]) to refresh its token. That is safe only
// because the home holds nothing of the user's; a test pins it to true.
func codexSandboxConfig(home string, prog externagent.Program) externagent.SandboxConfig {
	return externagent.SandboxConfig{
		AllowedHosts:    codexAllowedHosts,
		AuthDir:         home,
		AuthEnv:         "CODEX_HOME",
		AuthDirWritable: true,
		ProgramDirs:     prog.ReadableDirs,
		ProbeTimeout:    externalProbeTimeout,
		// The codex CLI canonicalizes paths at startup, which no process can do inside a
		// Windows AppContainer, so it runs with the host readable and writes confined to the
		// workspace. Writes, egress and tools stay gated, and the episode records the tier.
		HostReadable: true,
	}
}

// claudeSandboxConfig lets the CLI write its home
// ([externagent.SandboxConfig.AuthDirWritable]) to refresh its token. That is safe only
// because the home holds nothing of the user's; a test pins it to true.
func claudeSandboxConfig(home string, prog externagent.Program) externagent.SandboxConfig {
	return externagent.SandboxConfig{
		AllowedHosts:    claudeAllowedHosts,
		AuthDir:         home,
		AuthEnv:         "CLAUDE_CONFIG_DIR",
		AuthDirWritable: true,
		ProgramDirs:     prog.ReadableDirs,
		ProbeTimeout:    externalProbeTimeout,
		// Claude Code canonicalizes paths at startup, which no process can do inside a Windows
		// AppContainer, so it runs with the host readable and writes confined to the workspace.
		// Writes, egress and tools stay gated, and the episode records the tier.
		HostReadable: true,
	}
}

// externalAdapter builds the adapter that detects the backend's CLI and drives one
// episode over spawner.
func externalAdapter(name string, spawner externagent.Spawner) (externagent.Adapter, error) {
	switch name {
	case "codex":
		// Launch the native executable behind the npm launcher: a script needs its interpreter
		// inside the confinement, and an unprivileged process cannot grant one under a
		// system-owned directory. An absent CLI leaves the name for Detect to report.
		prog, err := externagent.LocateCodex("")
		if err != nil {
			if !errors.Is(err, externagent.ErrProgramNotFound) {
				return nil, err
			}
			return externagent.NewCodex("", spawner), nil
		}
		return externagent.NewCodex(prog.Path, spawner), nil
	case "claude":
		// Launch the native binary behind the npm launcher, whose shim needs a shell inside the
		// confinement. An absent CLI leaves the name for Detect to report.
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

// newExternalAgent builds the backend's driver for workdir. Adapter and driver share one
// spawner, so probes and episodes run under the same confinement. It does not probe;
// resolveExternalAgent does.
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

// resolveExternalAgent returns the driver when the CLI is installed, signed in and new
// enough to constrain. Errors follow [readinessError].
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
	r = withHomeLogin(name, r)
	if rerr := readinessError(name, r); rerr != nil {
		return nil, rerr
	}
	// An episode forces the CLI's egress through the governed proxy and would refuse
	// mid-run without it. Checking here gives one message naming a distribution that can
	// run it.
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

// readinessError returns nil for a ready CLI. A Refuse is terminal: the CLI cannot be
// made to route its effects through the bridge. An onboarding gap (not installed, not
// signed in) returns the adapter's own Reason, such as "run `codex login`". No API key
// is asked for, since an external agent uses its own subscription.
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
