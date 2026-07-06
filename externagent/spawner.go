package externagent

import (
	"context"
	"fmt"
	"io"
	"os"
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
	// bridge is not listed here: the child reaches it directly, outside the proxy, so the
	// bridge stays reachable while the internet does not. An empty list permits no egress
	// at all beyond the bridge (deny-all), enough for an offline detection run but not a
	// live episode. These are supplied by the adapter, so no provider name is baked into
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

	opts := []sandbox.LocalOption{sandbox.WithDefaultConfinement()}
	if s.cfg.AuthDir != "" {
		opts = append(opts, sandbox.WithReadableDir(s.cfg.AuthDir))
	}
	if s.cfg.ProbeTimeout > 0 {
		opts = append(opts, sandbox.WithExecTimeout(s.cfg.ProbeTimeout))
	}
	loc, err := sandbox.NewLocal(root, opts...)
	if err != nil {
		return "", fmt.Errorf("externagent: probe sandbox: %w", err)
	}
	defer func() { _ = loc.Close() }()

	res, err := loc.Capture(ctx, sandbox.CaptureSpec{Argv: append([]string{path}, args...)})
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
	loc, err := sandbox.NewLocal(ep.Workdir, s.episodeOptions()...)
	if err != nil {
		return nil, fmt.Errorf("externagent: episode sandbox: %w", err)
	}
	if got := loc.Containment(); got < s.cfg.MinContainment {
		_ = loc.Close()
		return nil, fmt.Errorf("externagent: host containment is %s, below the required %s; refusing to start an untrusted harness less contained than required", got, s.cfg.MinContainment)
	}
	proc, err := loc.Stream(ctx, sandbox.StreamSpec{
		Argv:    append([]string{inv.Path}, inv.Args...),
		Stdin:   []byte(inv.Stdin),
		Env:     inv.Env,
		Confine: true,
	})
	if err != nil {
		_ = loc.Close()
		return nil, err
	}
	return &confinedProcess{proc: proc, closer: loc}, nil
}

// episodeOptions is the sandbox configuration for a live episode: the kernel-confined
// tier (a read-only host and the syscall filter) plus the governed egress gate and the
// auth-home read grant. Network denial is deliberately not set: egress is governed by the
// proxy gate, not blocked wholesale, so the child can still reach the allowlisted
// provider and the loopback bridge.
func (s *SandboxSpawner) episodeOptions() []sandbox.LocalOption {
	opts := []sandbox.LocalOption{
		sandbox.WithReadOnlyFS(),
		sandbox.WithSeccomp(),
		sandbox.WithEgress(episodePolicy(s.cfg.AllowedHosts)),
	}
	if s.cfg.AuthDir != "" {
		opts = append(opts, sandbox.WithReadableDir(s.cfg.AuthDir))
	}
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
	proc   *sandbox.StreamProcess
	closer io.Closer
}

// Stdout is the confined child's live standard output.
func (p *confinedProcess) Stdout() io.Reader { return p.proc.Stdout() }

// Wait blocks until the child exits, then releases the per-episode sandbox. It returns
// the process's outcome; a close error is surfaced only when the process itself exited
// cleanly, so a real run failure is never masked by a teardown error.
func (p *confinedProcess) Wait() error {
	err := p.proc.Wait()
	if cerr := p.closer.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
