package externagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/sandbox"
)

// SandboxConfig is the confinement a SandboxSpawner runs an external CLI under. The
// harness is untrusted and its own sandbox is bypassed, so this profile is the only
// boundary. The zero value refuses a live episode: no provider egress, kernel
// containment required.
type SandboxConfig struct {
	// AllowedHosts are the only names the child may reach through the egress proxy:
	// the provider's API and auth endpoints. A listed name that resolves to a private
	// or rebinding address is still denied. A leading dot (".example.com") matches any
	// subdomain. The loopback MCP bridge is not listed because it bypasses the proxy:
	// the child dials it directly, or through the in-namespace forward Start sets up.
	// Empty permits no egress beyond the bridge, enough for detection but not an
	// episode. The adapter supplies the list, so no provider name is baked in here.
	AllowedHosts []string
	// AuthDir is the CLI's credential and config home, outside the workspace. The
	// child gets read on it for the episode and the grant is revoked on teardown, so the
	// credential is never copied into the workspace, vault or record. Empty grants no
	// read. It only takes effect where reads are denied by default (a Windows
	// AppContainer); a read-only host on Linux and macOS already permits it.
	AuthDir string
	// AuthEnv is the variable the CLI reads to find its home (CODEX_HOME for codex).
	// The child inherits no HOME or USERPROFILE, so without it the CLI looks elsewhere
	// and reports itself logged out. Empty passes no variable.
	AuthEnv string
	// AuthSeedPaths are absolute paths of files, possibly in different directories,
	// assembled by base name into one per-episode home, for a CLI whose config and
	// token are split (Claude Code keeps config in the home directory and the token in
	// a subdirectory). It takes precedence over AuthSeedFiles. A missing file is
	// skipped, so detection reports a partial setup as not ready. The run writes and
	// deletes the assembled copy, so a refreshed token lives only there and the host
	// credential is never writable to the harness.
	AuthSeedPaths []string
	// AuthSeedFiles are the files inside AuthDir that each episode gets a writable
	// copy of, in a directory outside the workspace (for codex: the OAuth token and the
	// config). The CLI writes its home as it runs and every tier denies writes outside
	// the workspace, so a read-only AuthDir fails mid-episode. Write on AuthDir would let
	// the harness corrupt the credential, and a home inside the workspace would put the
	// token in the record. A mid-episode refresh touches only the copy, so the host's
	// refresh token stays valid. Empty keeps AuthDir read-only, as a detection probe needs.
	AuthSeedFiles []string
	// AuthDirWritable grants the child write on AuthDir itself. Set it only for a home
	// that exists for this program's use of the CLI, signed in with the CLI's own login,
	// so the worst a harness can do is force a fresh sign-in. Never set it for the user's
	// own CLI home. It has no effect when seeds are configured.
	AuthDirWritable bool
	// ProgramDirs hold the CLI's executable and its helper binaries. The child gets
	// read and execute on them for the launch, because confinement denies reads by
	// default where the platform enforces it. They come from resolving the CLI to its
	// native executable (see LocateCodex), so a launcher script's interpreter under a
	// system-owned directory, which an unprivileged process cannot grant, never enters
	// the confinement. Empty grants nothing.
	ProgramDirs []string
	// HostReadable selects the tier that confines writes to the workspace but leaves the
	// host readable (see sandbox.WithHostReadable). Codex needs it: any Rust program that
	// canonicalizes a path fails in a Windows AppContainer, whose token cannot map a file
	// handle back to a DOS path. The harness can then read the user's files but still
	// writes nothing outside the workspace and its egress stays gated, so this is set per
	// backend and the tier that ran is recorded. It changes nothing on Linux and macOS,
	// where the kernel tier already leaves the host readable.
	HostReadable bool
	// MinContainment is the floor the host must enforce or the episode is refused, so an
	// untrusted harness never drops to a weaker boundary. Zero means
	// sandbox.ContainmentKernel; a caller can raise it (a microVM tier) but not lower it.
	MinContainment sandbox.Containment
	// ProbeTimeout caps a detection probe so a hung CLI cannot stall detection. Zero
	// leaves only the caller's context.
	ProbeTimeout time.Duration
}

// SandboxSpawner is the production Spawner. It runs an external CLI as an untrusted
// subprocess through the sandbox's launch functions (os/exec is confined to the sandbox
// package), with a read-only host, the syscall filter, provider-only egress and a read
// grant on the CLI's auth home. Probe runs detection; Start runs an episode bound to the
// context, so a halt kills the CLI.
type SandboxSpawner struct {
	cfg SandboxConfig

	// mu guards runHome, the credential-and-state home this run's episodes share.
	mu      sync.Mutex
	runHome string
}

// NewSandboxSpawner builds the production Spawner. A zero MinContainment becomes
// sandbox.ContainmentKernel.
func NewSandboxSpawner(cfg SandboxConfig) *SandboxSpawner {
	if cfg.MinContainment == sandbox.ContainmentNone {
		cfg.MinContainment = sandbox.ContainmentKernel
	}
	return &SandboxSpawner{cfg: cfg}
}

var _ Spawner = (*SandboxSpawner)(nil)

// ForwardBridge returns the address the confined child should dial for a bridge on the
// host loopback, and the host address to forward it to. On Linux the child has its own
// network namespace, so the bridge is forwarded in; where the child shares the host's
// stack the URL is returned unchanged with no forward. The runner calls it before
// building the episode command.
func (s *SandboxSpawner) ForwardBridge(hostURL string) (childURL, forwardTo string) {
	return sandbox.ForwardBridge(hostURL)
}

// authEnv points the child at its credential home. The value is a path; the token stays
// in the directory and never passes through the environment, command line or record.
func (s *SandboxSpawner) authEnv(home string) []string {
	if s.cfg.AuthEnv == "" || home == "" {
		return nil
	}
	return []string{s.cfg.AuthEnv + "=" + home}
}

// episodeAuthHome returns the episode's CLI home and the seed files copied into it,
// which must be deleted when the episode ends. With no seeds it is AuthDir, read-only,
// with nothing to delete.
//
// The home is per run, beside the workspace (never inside it, which the record
// captures), because it holds the CLI's conversation and a later turn continues it.
// Credentials are copied in per episode and deleted when it ends, so a token never
// outlives its episode. A missing seed is skipped: authentication is Probe's question.
func (s *SandboxSpawner) episodeAuthHome(workdir string) (string, []string, error) {
	srcs, err := s.seedSources()
	if err != nil {
		return "", nil, err
	}
	if len(srcs) == 0 {
		return s.cfg.AuthDir, nil, nil
	}
	home, err := s.ensureRunHome(workdir)
	if err != nil {
		return "", nil, err
	}
	seeded := make([]string, 0, len(srcs))
	for _, src := range srcs {
		dst := filepath.Join(home, filepath.Base(src))
		if err := copyIfPresent(src, dst); err != nil {
			removeSeeds(seeded)
			return "", nil, fmt.Errorf("externagent: seed credential home: %w", err)
		}
		seeded = append(seeded, dst)
	}
	return home, seeded, nil
}

// seedSources lists the files to copy into the home, from AuthSeedPaths or from
// AuthDir plus AuthSeedFiles.
func (s *SandboxSpawner) seedSources() ([]string, error) {
	if len(s.cfg.AuthSeedPaths) > 0 {
		return s.cfg.AuthSeedPaths, nil
	}
	if s.cfg.AuthDir == "" || len(s.cfg.AuthSeedFiles) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(s.cfg.AuthSeedFiles))
	for _, name := range s.cfg.AuthSeedFiles {
		// A seed must name one file directly inside AuthDir, so a name like
		// "../../.ssh/id_ed25519" cannot pull a file into a directory the harness writes.
		if name != filepath.Base(name) || name == "." || name == ".." {
			return nil, fmt.Errorf("externagent: credential seed %q must be a file name, not a path", name)
		}
		out = append(out, filepath.Join(s.cfg.AuthDir, name))
	}
	return out, nil
}

// ensureRunHome creates the run's home on first use and returns it on every later call.
func (s *SandboxSpawner) ensureRunHome(workdir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runHome != "" {
		return s.runHome, nil
	}
	home, err := os.MkdirTemp(filepath.Dir(workdir), "flynn-authhome-")
	if err != nil {
		return "", fmt.Errorf("externagent: credential home: %w", err)
	}
	s.runHome = home
	return home, nil
}

// Close removes the run's home and the CLI's conversations in it. Call it when the run
// ends; credentials were already deleted with each episode.
func (s *SandboxSpawner) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runHome == "" {
		return nil
	}
	err := os.RemoveAll(s.runHome)
	s.runHome = ""
	return err
}

// assembleSeedHome copies each source file by base name into a fresh directory under
// parent. A missing source is skipped. Sources must have distinct base names; a later
// one overwrites an earlier one.
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

// copyIfPresent copies one seed file, doing nothing when the source is missing. The
// copy is owner-only because it is usually an OAuth token.
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
	return fsatomic.WriteFile(dst, data, 0o600)
}

// Probe runs path with args to completion and returns its combined output, for a
// version or auth-status check. It uses baseline confinement, which degrades to the
// process jail instead of refusing, so detection works wherever the CLI is installed;
// only Start refuses. A non-zero exit returns an error with the output kept, so the
// adapter can parse why the CLI is absent or not ready.
func (s *SandboxSpawner) Probe(ctx context.Context, path string, args ...string) (string, error) {
	root, err := os.MkdirTemp("", "flynn-externagent-probe-")
	if err != nil {
		return "", fmt.Errorf("externagent: probe workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	// A split-layout CLI gets the same assembled home an episode would; otherwise
	// the probe reads AuthDir directly.
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

// Start launches one episode's subprocess and returns its stdout and a wait handle. The
// child gets a read-only host and the syscall filter, so the OS refuses its native
// writes; provider-only egress, so its direct provider channel is contained though not
// observed; and a read grant on its auth home. The launch is refused if the host cannot
// meet MinContainment. Cancelling ctx kills the CLI, and the per-episode sandbox is
// released when Wait returns.
func (s *SandboxSpawner) Start(ctx context.Context, ep Episode, inv Invocation) (Process, error) {
	home, seeded, err := s.episodeAuthHome(ep.Workdir)
	if err != nil {
		return nil, err
	}
	// The home outlives the episode; the credentials copied into it do not.
	writable := s.homeWritable(seeded)
	// A per-episode writable temp outside the workspace. A read-only host leaves the
	// system temp unwritable, and a runtime that needs scratch on startup (a
	// compiled JavaScript runtime) exits silently without it.
	tmpDir, err := os.MkdirTemp(filepath.Dir(ep.Workdir), "flynn-tmp-")
	if err != nil {
		removeSeeds(seeded)
		return nil, fmt.Errorf("externagent: episode temp dir: %w", err)
	}
	opts := s.episodeOptions(home, writable)
	opts = append(opts, sandbox.WithWritableDir(tmpDir))
	// A child in its own network namespace cannot reach the host-loopback bridge, so
	// that one address is forwarded in and nothing else on the host loopback is
	// reachable. ForwardTo is empty where the child reaches the bridge directly.
	if ep.Bridge.ForwardTo != "" {
		opts = append(opts, sandbox.WithLoopbackForward(ep.Bridge.ForwardTo))
	}
	loc, err := sandbox.NewLocal(ep.Workdir, opts...)
	if err != nil {
		removeSeeds(seeded)
		removeScratch(tmpDir)
		return nil, fmt.Errorf("externagent: episode sandbox: %w", err)
	}
	if got := loc.Containment(); got < s.cfg.MinContainment {
		_ = loc.Close()
		removeSeeds(seeded)
		removeScratch(tmpDir)
		return nil, fmt.Errorf("externagent: host containment is %s, below the required %s; refusing to start an untrusted harness less contained than required", got, s.cfg.MinContainment)
	}
	tier := loc.ConfinementTier()
	env := slices.Concat(inv.Env, s.authEnv(home))
	env = append(env, tempEnv(tmpDir)...)
	proc, err := loc.Stream(ctx, sandbox.StreamSpec{
		Argv:    append([]string{inv.Path}, inv.Args...),
		Stdin:   []byte(inv.Stdin),
		Env:     env,
		Confine: true,
	})
	if err != nil {
		_ = loc.Close()
		removeSeeds(seeded)
		removeScratch(tmpDir)
		return nil, err
	}
	return &confinedProcess{proc: proc, closer: loc, tier: tier, seeds: seeded, tmpScratch: tmpDir}, nil
}

// homeWritable reports whether the child may write its home: the run's copy when seeds
// were copied, or an AuthDirWritable home. A user's own CLI home stays read-only.
func (s *SandboxSpawner) homeWritable(seeded []string) bool {
	return len(seeded) > 0 || s.cfg.AuthDirWritable
}

// tempEnv sets TMPDIR, TMP and TEMP to the per-episode temp, so one launch works on
// Unix and Windows.
func tempEnv(dir string) []string {
	return []string{"TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir}
}

// removeSeeds deletes one episode's copied credentials. It runs on every exit from a
// launch and again when the child exits.
func removeSeeds(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

func removeScratch(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// episodeOptions is the sandbox configuration for a live episode. Network denial is not
// set because the proxy gate governs egress, and the child must still reach the
// provider and the loopback bridge. The home is writable only when it is this Spawner's
// copy or an AuthDirWritable home, so a harness cannot rewrite the user's own credential.
func (s *SandboxSpawner) episodeOptions(home string, writable bool) []sandbox.LocalOption {
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
		if writable {
			opts = append(opts, sandbox.WithWritableDir(home))
		}
	}
	opts = append(opts, sandbox.WithReadableDir(s.cfg.ProgramDirs...))
	return opts
}

// episodePolicy allows egress only to the listed hosts. AllowPublic passes their public
// addresses while private, loopback and metadata addresses stay denied, so a listed name
// that rebinds privately is refused. An empty list means no public egress at all.
func episodePolicy(hosts []string) netguard.Policy {
	if len(hosts) == 0 {
		return netguard.DenyAll()
	}
	return netguard.Policy{AllowPublic: true, AllowHosts: hosts}
}

// confinedProcess is a running episode subprocess. Its Wait also releases the
// per-episode sandbox, so the proxy stops and grants are revoked once the process ends,
// including after a kill.
type confinedProcess struct {
	proc       *sandbox.StreamProcess
	closer     io.Closer
	tier       string
	seeds      []string // the credentials copied in for this episode, deleted when it ends
	tmpScratch string   // the per-episode temp directory to delete
}

// Stdout is the child's live standard output.
func (p *confinedProcess) Stdout() io.Reader { return p.proc.Stdout() }

// ConfinementTier names the mechanism that confined this child, so the record states
// the boundary it ran behind. The two Windows tiers confine reads differently (see
// sandbox.WithHostReadable), which the containment level alone does not show.
func (p *confinedProcess) ConfinementTier() string { return p.tier }

// Wait waits for the child, then releases the sandbox. A close error is returned only
// when the process exited cleanly, so it never masks a run failure.
func (p *confinedProcess) Wait() error {
	err := p.proc.Wait()
	if cerr := p.closer.Close(); cerr != nil && err == nil {
		err = cerr
	}
	// After Close, so the grants are dropped before the token is deleted. The home stays
	// for the next turn's conversation.
	removeSeeds(p.seeds)
	removeScratch(p.tmpScratch)
	return err
}
