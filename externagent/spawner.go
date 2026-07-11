package externagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/sandbox"
)

// SandboxConfig is the confinement envelope a SandboxSpawner runs an external CLI
// under. The external harness is untrusted: its own sandbox is bypassed and is not the
// boundary, so this profile is the only one. A zero value refuses a live episode (no
// provider egress, kernel containment required), which is the safe default.
type SandboxConfig struct {
	// AllowedHosts is the destination-name allowlist for an episode's egress: the
	// external provider's API and auth endpoints, the only names the confined child may
	// reach out to. It is a name gate on the egress proxy ("deny all egress except these
	// providers"): a name not on the list is denied, and a listed name that resolves to a
	// private or rebinding address is denied too (the address gate still applies). An
	// entry beginning with a dot (".example.com") matches any subdomain. The loopback MCP
	// bridge is not listed here: it is not reached through the egress proxy at all. Where the
	// child shares the host's network stack it dials the bridge's host loopback directly;
	// where the child is confined to its own network namespace it dials an in-namespace
	// address the sandbox forwards to the bridge (see the loopback forward in Start). Either
	// way the bridge stays reachable while the internet does not. An empty list permits no
	// egress at all beyond the bridge (deny-all), enough for an offline detection run but not
	// a live episode. These are supplied by the adapter, so no provider name is baked into
	// this generic host.
	AllowedHosts []string
	// AuthDir is the external CLI's credential and config home (its OAuth token lives
	// there), which sits outside the episode workspace. The confined child is granted
	// read (and traverse) on it for the life of the episode and the grant is revoked on
	// teardown, so the credential stays in its home directory and is never copied into the
	// workspace, the vault, or the record. Empty grants no extra read. On Linux and macOS
	// a read-only host already permits the read, so this takes effect only where the
	// confinement denies reads by default (a Windows AppContainer).
	AuthDir string
	// AuthEnv is the environment variable the external CLI reads to find its credential
	// and config home (CODEX_HOME for codex). The confined child inherits none of the
	// host's environment, so it has no HOME or USERPROFILE to derive that home from and
	// would look for its credentials somewhere that does not exist, reporting itself
	// logged out on a host where it is logged in. Granting AuthDir makes the directory
	// readable; naming it here is what makes the child look in it. Empty passes no
	// variable.
	AuthEnv string
	// AuthSeedPaths names individual source files, by absolute path, that together make up
	// the CLI's credential and config home. It is the multi-source counterpart of AuthDir
	// plus AuthSeedFiles: where those copy named files out of one directory, this gathers
	// files that live in different directories (or under a home the confined child has no
	// way to derive) into one directory holding exactly their base names and nothing else.
	// A CLI whose credential and config are split across two locations (Claude Code keeps
	// its config in the home directory and its OAuth token in a subdirectory) is given one
	// directory it can be pointed at, for both detection and an episode. When set it takes
	// precedence over AuthSeedFiles; AuthEnv still names the variable that points the child
	// at the assembled directory, and a source file that does not exist is skipped so a
	// partially configured CLI assembles what it has and detection reports the rest as
	// not-ready. Like the AuthSeedFiles copy, the assembled directory is a per-episode home
	// the run writes and deletes, so a token the harness refreshes lives only in the copy
	// and the host credential is never made writable to it.
	AuthSeedPaths []string
	// AuthSeedFiles names the files inside AuthDir that an episode's credential home must
	// contain (for codex: the OAuth token and the CLI's own config). Naming them switches
	// an episode from pointing the child straight at AuthDir to giving it a per-episode
	// copy of just these files, in a writable directory outside the workspace that the run
	// creates and deletes.
	//
	// An episode needs this because the CLI writes its own home as it runs (a session
	// rollout, a log, a PATH shim) and every confinement tier denies writes outside the
	// workspace, so pointing it at a read-only AuthDir fails partway through the episode.
	// The two obvious fixes are both worse. Granting write on the host's AuthDir hands an
	// untrusted harness the credential file it authenticates with, to corrupt or replace,
	// and leaves whatever it wrote behind after the run. Putting the home inside the
	// workspace copies the OAuth token into the tree the record captures. The per-episode
	// copy has neither problem: the child can write all it likes, it writes only a copy
	// that dies with the episode, and the copy sits outside the recorded workspace.
	//
	// The cost is that a token the CLI refreshes mid-episode is refreshed only in the copy,
	// so the host's credential stays as it was; the refresh token it was issued from
	// remains valid, so the next episode still authenticates. Empty keeps the read-only
	// AuthDir behaviour (correct for a detection probe, which writes nothing).
	AuthSeedFiles []string
	// ProgramDirs are the directories holding the external CLI's own executable and the
	// helper binaries shipped beside it. The confined child is granted read (and execute)
	// on them for the life of the launch and the grant is revoked on teardown. Without it
	// the child cannot load the very program it is meant to run: confinement is
	// default-deny for reads where the platform enforces it, and the CLI lives outside the
	// episode workspace. They come from resolving the CLI to its native executable (see
	// LocateCodex), so a launcher script's interpreter, which may sit under a system-owned
	// directory an unprivileged process cannot grant at all, never enters the confinement.
	// Empty grants no extra read, which is correct where the confinement leaves the host
	// filesystem readable.
	ProgramDirs []string
	// HostReadable selects the confinement tier that confines the child's writes to the
	// episode workspace but leaves the host filesystem readable to it (see
	// sandbox.WithHostReadable). It is set for a CLI whose runtime cannot start under a
	// deny-by-default read posture: the codex CLI is a Rust program, and every Rust program
	// that canonicalizes a path fails inside a Windows AppContainer, whose token cannot
	// perform the final step that maps a file handle back to a DOS path. The weakening is
	// real (the harness can read the host user's files) and bounded (it can still write
	// nothing outside the workspace, and its egress is still gated), so it is named per
	// backend rather than defaulted, and the tier that ran is recorded on the episode. On
	// Linux and macOS the kernel tier already leaves the host readable, so this changes
	// nothing there.
	HostReadable bool
	// MinContainment is the floor the host must actually enforce or an episode is refused
	// rather than run less contained (refuse-rather-than-downgrade: an untrusted harness
	// never silently drops to a weaker boundary). The zero value is treated as
	// sandbox.ContainmentKernel, the boundary for semi-trusted, model-authored code over a
	// shared kernel; a caller can raise it (a microVM tier) but not lower it below the
	// kernel floor by leaving it zero.
	MinContainment sandbox.Containment
	// ProbeTimeout caps how long a detection probe may run before it is killed, so a
	// hung CLI cannot stall detection. Zero applies no cap beyond the caller's context.
	ProbeTimeout time.Duration
}

// SandboxSpawner is the production Spawner: it runs an external agent CLI as an
// untrusted subprocess inside the sandbox's containment envelope, the single security
// boundary around a harness whose own code the run does not control. It backs the
// Spawner port with the sandbox's streaming and capture launch primitives (no direct
// os/exec, which the repo confines to the sandbox package), composing a read-only host,
// the syscall filter, a deny-all-except-provider egress gate, and a read grant for the
// CLI's own auth home into one launch. Detection runs through Probe; an episode runs
// through Start, whose process is bound to the context so a halt kills the CLI.
type SandboxSpawner struct {
	cfg SandboxConfig
}

// NewSandboxSpawner builds the production Spawner for the given confinement envelope. A
// MinContainment left at the zero value is raised to sandbox.ContainmentKernel, so the
// default is the kernel-confined floor rather than an unconfined process jail.
func NewSandboxSpawner(cfg SandboxConfig) *SandboxSpawner {
	if cfg.MinContainment == sandbox.ContainmentNone {
		cfg.MinContainment = sandbox.ContainmentKernel
	}
	return &SandboxSpawner{cfg: cfg}
}

var _ Spawner = (*SandboxSpawner)(nil)

// ForwardBridge reports how the confined child reaches a bridge on the host loopback. The
// child runs behind the sandbox's network confinement, which on Linux is a separate network
// namespace whose loopback is not the host's, so the bridge is forwarded in: the child is
// given an in-namespace address and the sandbox forwards it to the host one. Where the child
// shares the host's stack the sandbox reports the host URL unchanged and no forward. The
// runner calls this before building the episode command, so the child is configured with an
// address it can actually reach.
func (s *SandboxSpawner) ForwardBridge(hostURL string) (childURL, forwardTo string) {
	return sandbox.ForwardBridge(hostURL)
}

// authEnv is the environment grant that points the confined child at its own credential
// home. It is the counterpart of the read grant on AuthDir: the grant makes the directory
// readable, this makes the CLI look there. The value is a path, never a credential; the
// token itself stays in the directory and is never passed through the environment, the
// command line, or the record.
func (s *SandboxSpawner) authEnv(home string) []string {
	if s.cfg.AuthEnv == "" || home == "" {
		return nil
	}
	return []string{s.cfg.AuthEnv + "=" + home}
}

// episodeAuthHome returns the directory an episode's child should use as its credential
// home, and true when that directory is a per-episode copy this Spawner created and must
// delete. With no AuthSeedFiles configured it is the host's AuthDir, read-only and owned
// by nobody here. Otherwise it is a fresh directory beside the workspace (never inside it,
// which the record captures) holding a copy of the seed files and nothing else.
//
// A seed file that does not exist is skipped rather than failing: a CLI that has never
// been configured has no config file, and whether it is authenticated at all is the
// adapter's question to answer through Probe, not a launch precondition here.
func (s *SandboxSpawner) episodeAuthHome(workdir string) (string, bool, error) {
	if len(s.cfg.AuthSeedPaths) > 0 {
		home, err := assembleSeedHome(filepath.Dir(workdir), s.cfg.AuthSeedPaths)
		if err != nil {
			return "", false, err
		}
		return home, true, nil
	}
	if s.cfg.AuthDir == "" || len(s.cfg.AuthSeedFiles) == 0 {
		return s.cfg.AuthDir, false, nil
	}
	home, err := os.MkdirTemp(filepath.Dir(workdir), "flynn-authhome-")
	if err != nil {
		return "", false, fmt.Errorf("externagent: episode credential home: %w", err)
	}
	for _, name := range s.cfg.AuthSeedFiles {
		// A seed names one file directly inside AuthDir. Rejecting anything else keeps the
		// copy from reaching outside the credential home in either direction: a name like
		// "../../.ssh/id_ed25519" would otherwise pull a file the harness was never meant
		// to see into a directory it can write.
		if name != filepath.Base(name) || name == "." || name == ".." {
			_ = os.RemoveAll(home)
			return "", false, fmt.Errorf("externagent: credential seed %q must be a file name, not a path", name)
		}
		if err := copyIfPresent(filepath.Join(s.cfg.AuthDir, name), filepath.Join(home, name)); err != nil {
			_ = os.RemoveAll(home)
			return "", false, fmt.Errorf("externagent: seed credential home: %w", err)
		}
	}
	return home, true, nil
}

// assembleSeedHome builds a credential home from a set of individual source files, each
// copied by its base name into a fresh directory created under parent. It is the
// multi-source counterpart of the AuthDir-plus-AuthSeedFiles copy, for a CLI whose
// credential and config live in different directories. A source that does not exist is
// skipped rather than failing, so a partially configured CLI still assembles what it has.
// The base names of the sources must be distinct; a later source with the same base name
// overwrites an earlier one.
func assembleSeedHome(parent string, paths []string) (string, error) {
	home, err := os.MkdirTemp(parent, "flynn-authhome-")
	if err != nil {
		return "", fmt.Errorf("externagent: assemble credential home: %w", err)
	}
	for _, src := range paths {
		if err := copyIfPresent(src, filepath.Join(home, filepath.Base(src))); err != nil {
			_ = os.RemoveAll(home)
			return "", fmt.Errorf("externagent: seed credential home: %w", err)
		}
	}
	return home, nil
}

// copyIfPresent copies one seed file, doing nothing when the source does not exist. The
// destination is created readable and writable by its owner alone, because the file it
// most often carries is an OAuth token.
func copyIfPresent(src, dst string) error {
	//nolint:gosec // both paths are a caller-configured directory joined with a name the caller already constrained to a single path element, so neither can escape the credential home
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	//nolint:gosec // dst is the per-episode home joined with that same constrained name
	return os.WriteFile(dst, data, 0o600)
}

// Probe runs path with args to completion under best-effort confinement and returns its
// combined output, for detection (a version or auth-status probe). Detection must work
// wherever the CLI is installed, so the probe uses the always-on baseline confinement
// that degrades to the process-jail floor rather than refusing on a host that cannot set
// up the kernel tier; the episode path (Start) is the one that refuses. The CLI's auth
// home is granted read so an auth-status probe can see whether it is logged in. A
// non-zero exit is returned as an error with the output preserved, so detection reads it
// as "not present" or "not ready" while the adapter can still parse the reason.
func (s *SandboxSpawner) Probe(ctx context.Context, path string, args ...string) (string, error) {
	root, err := os.MkdirTemp("", "flynn-externagent-probe-")
	if err != nil {
		return "", fmt.Errorf("externagent: probe workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	// The credential home the probe points the CLI at: for a split-layout CLI it is a
	// combined directory assembled from the seed paths, so an auth-status probe sees the
	// same home an episode would; otherwise it is the host's own AuthDir, read-only.
	authHome := s.cfg.AuthDir
	opts := []sandbox.LocalOption{sandbox.WithDefaultConfinement()}
	if s.cfg.HostReadable {
		opts = append(opts, sandbox.WithHostReadable())
	}
	switch {
	case len(s.cfg.AuthSeedPaths) > 0:
		home, err := assembleSeedHome(root, s.cfg.AuthSeedPaths)
		if err != nil {
			return "", err
		}
		authHome = home
		opts = append(opts, sandbox.WithReadableDir(home))
	case s.cfg.AuthDir != "":
		opts = append(opts, sandbox.WithReadableDir(s.cfg.AuthDir))
	}
	opts = append(opts, sandbox.WithReadableDir(s.cfg.ProgramDirs...))
	if s.cfg.ProbeTimeout > 0 {
		opts = append(opts, sandbox.WithExecTimeout(s.cfg.ProbeTimeout))
	}
	loc, err := sandbox.NewLocal(root, opts...)
	if err != nil {
		return "", fmt.Errorf("externagent: probe sandbox: %w", err)
	}
	defer func() { _ = loc.Close() }()

	res, err := loc.Capture(ctx, sandbox.CaptureSpec{Argv: append([]string{path}, args...), Env: s.authEnv(authHome)})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Output, fmt.Errorf("externagent: probe %q exited with code %d", path, res.ExitCode)
	}
	return res.Output, nil
}

// Start launches one episode's subprocess inside the containment envelope and returns
// its live stdout and a wait handle. The child runs with a read-only host and the
// syscall filter (so its bypassed native writes and dangerous syscalls are refused by
// the OS, not by trusting the CLI), a deny-all-except-provider egress gate (so its only
// way out is the allowlisted provider, and its direct provider channel stays
// unobserved-but-contained), and a read grant for its auth home. Before it launches, the
// host's actual containment is checked against the configured floor and the launch is
// refused if the host cannot meet it, so an untrusted harness never runs less contained
// than required. The process is bound to ctx: cancelling it (a halt or shutdown) kills
// the CLI, and the per-episode sandbox (its egress proxy and read grant) is released when
// Wait returns.
func (s *SandboxSpawner) Start(ctx context.Context, ep Episode, inv Invocation) (Process, error) {
	home, owned, err := s.episodeAuthHome(ep.Workdir)
	if err != nil {
		return nil, err
	}
	scratch := ""
	if owned {
		scratch = home
	}
	// A per-episode writable temp directory outside the recorded workspace. A read-only host
	// leaves the system temp directory unwritable, and a CLI whose runtime needs scratch
	// space (a compiled JavaScript runtime writes to its temp on startup) exits without a
	// word when it cannot get any, so the episode never produces a line. Handing it a
	// writable temp of its own, pointed at by the standard temp variables, keeps that scratch
	// off the host and out of the workspace the record captures, and it dies with the episode.
	tmpDir, err := os.MkdirTemp(filepath.Dir(ep.Workdir), "flynn-tmp-")
	if err != nil {
		removeScratch(scratch)
		return nil, fmt.Errorf("externagent: episode temp dir: %w", err)
	}
	opts := s.episodeOptions(home, owned)
	opts = append(opts, sandbox.WithWritableDir(tmpDir))
	// When the child is confined to its own network namespace, the host-loopback bridge is
	// unreachable from inside it, so the run's bridge is forwarded in: the child dials an
	// in-namespace address the sandbox pipes to the one host address named here, and nothing
	// else on the host loopback is reachable. ForwardTo is empty where the child reaches the
	// bridge directly, in which case no forward is set up.
	if ep.Bridge.ForwardTo != "" {
		opts = append(opts, sandbox.WithLoopbackForward(ep.Bridge.ForwardTo))
	}
	loc, err := sandbox.NewLocal(ep.Workdir, opts...)
	if err != nil {
		removeScratch(scratch)
		removeScratch(tmpDir)
		return nil, fmt.Errorf("externagent: episode sandbox: %w", err)
	}
	if got := loc.Containment(); got < s.cfg.MinContainment {
		_ = loc.Close()
		removeScratch(scratch)
		removeScratch(tmpDir)
		return nil, fmt.Errorf("externagent: host containment is %s, below the required %s; refusing to start an untrusted harness less contained than required", got, s.cfg.MinContainment)
	}
	tier := loc.ConfinementTier()
	env := append(inv.Env, s.authEnv(home)...)
	env = append(env, tempEnv(tmpDir)...)
	proc, err := loc.Stream(ctx, sandbox.StreamSpec{
		Argv:    append([]string{inv.Path}, inv.Args...),
		Stdin:   []byte(inv.Stdin),
		Env:     env,
		Confine: true,
	})
	if err != nil {
		_ = loc.Close()
		removeScratch(scratch)
		removeScratch(tmpDir)
		return nil, err
	}
	return &confinedProcess{proc: proc, closer: loc, tier: tier, scratch: scratch, tmpScratch: tmpDir}, nil
}

// tempEnv points the child's temp-directory variables at the per-episode writable temp, so a
// runtime that writes scratch on startup finds a directory it can write. Both the Unix name
// (TMPDIR) and the Windows names (TMP, TEMP) are set, so the same launch works on either.
func tempEnv(dir string) []string {
	return []string{"TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir}
}

// removeScratch deletes a per-episode credential home, and does nothing when the episode
// pointed the child at the host's AuthDir instead. It runs on every path out of a launch,
// so a copied token never outlives the episode it was copied for.
func removeScratch(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// episodeOptions is the sandbox configuration for a live episode: the kernel-confined
// tier (a read-only host and the syscall filter) plus the governed egress gate and the
// grants for the child's credential home. Network denial is deliberately not set: egress
// is governed by the proxy gate, not blocked wholesale, so the child can still reach the
// allowlisted provider and the loopback bridge.
//
// The credential home is granted write only when it is the per-episode copy this Spawner
// owns and deletes; the host's own AuthDir is never made writable, so an untrusted harness
// cannot rewrite the credential it authenticates with.
func (s *SandboxSpawner) episodeOptions(home string, owned bool) []sandbox.LocalOption {
	opts := []sandbox.LocalOption{
		sandbox.WithReadOnlyFS(),
		sandbox.WithSeccomp(),
		sandbox.WithEgress(episodePolicy(s.cfg.AllowedHosts)),
	}
	if s.cfg.HostReadable {
		opts = append(opts, sandbox.WithHostReadable())
	}
	if home != "" {
		opts = append(opts, sandbox.WithReadableDir(home))
		if owned {
			opts = append(opts, sandbox.WithWritableDir(home))
		}
	}
	opts = append(opts, sandbox.WithReadableDir(s.cfg.ProgramDirs...))
	return opts
}

// episodePolicy builds the egress policy for an episode: deny all egress except the
// allowlisted provider hosts. The name gate (AllowHosts) restricts which destinations
// the child may name; AllowPublic passes the address gate for their public endpoints
// while still denying private, loopback, and metadata addresses, so an allowlisted name
// that rebinds to a private address is refused. With no allowed hosts the policy is
// default-deny (no public egress at all), the safe floor rather than an accidental
// open-internet grant: an empty allowlist must never widen egress.
func episodePolicy(hosts []string) netguard.Policy {
	if len(hosts) == 0 {
		return netguard.DenyAll()
	}
	return netguard.Policy{AllowPublic: true, AllowHosts: hosts}
}

// confinedProcess is a running confined episode subprocess. It exposes the sandbox
// stream's stdout and wait, and folds releasing the per-episode sandbox into Wait, so
// the egress proxy is stopped and the auth-home read grant is revoked once the process
// ends (including after a context-driven kill).
type confinedProcess struct {
	proc       *sandbox.StreamProcess
	closer     io.Closer
	tier       string
	scratch    string // the per-episode credential home to delete, or empty
	tmpScratch string // the per-episode temp directory to delete
}

// Stdout is the confined child's live standard output.
func (p *confinedProcess) Stdout() io.Reader { return p.proc.Stdout() }

// ConfinementTier names the mechanism that actually confined this episode's child, so a
// run's record states the boundary it ran behind rather than the strongest one the
// platform has. The two Windows tiers bound a code-execution exploit alike but confine
// reads differently (see sandbox.WithHostReadable), and a record that named only the
// containment level could not tell a reader which one held.
func (p *confinedProcess) ConfinementTier() string { return p.tier }

// Wait blocks until the child exits, then releases the per-episode sandbox. It returns
// the process's outcome; a close error is surfaced only when the process itself exited
// cleanly, so a real run failure is never masked by a teardown error.
func (p *confinedProcess) Wait() error {
	err := p.proc.Wait()
	if cerr := p.closer.Close(); cerr != nil && err == nil {
		err = cerr
	}
	// After the sandbox is closed, so the grants on the credential home are dropped before
	// the copied token is deleted with it.
	removeScratch(p.scratch)
	removeScratch(p.tmpScratch)
	return err
}
