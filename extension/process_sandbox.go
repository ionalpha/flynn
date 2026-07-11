package extension

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/sandbox"
)

// defaultExtensionMemoryMiB and defaultExtensionMaxProcesses bound an extension process so
// a hostile or runaway one cannot exhaust host memory or fork-bomb. They are generous
// enough for a real tool-server yet a hard ceiling the job object enforces.
const (
	defaultExtensionMemoryMiB    = 512
	defaultExtensionMaxProcesses = 32
)

// SandboxLauncher is the production Launcher. It runs an extension binary inside a fresh
// sandbox at the kernel-confined tier: a read-only host, the syscall filter, a scrubbed
// deny-by-default environment (no secret ever reaches the process), memory and
// process-count caps, and deny-by-default egress restricted to exactly the effective
// allow-list. It requires confinement: on a host or platform that cannot contain the
// process, the launch is refused rather than downgraded, so an extension that cannot be
// contained never runs.
type SandboxLauncher struct {
	workRoot string
	limits   sandbox.ResourceLimits
	counter  atomic.Int64
}

// NewSandboxLauncher returns a launcher that creates each extension's scratch working
// directory under workRoot. The directory is a fresh, empty jail per launch and is removed
// when the connection is stopped.
func NewSandboxLauncher(workRoot string) *SandboxLauncher {
	return &SandboxLauncher{
		workRoot: workRoot,
		limits:   sandbox.ResourceLimits{MemoryMiB: defaultExtensionMemoryMiB, MaxProcesses: defaultExtensionMaxProcesses},
	}
}

// Launch starts req.Path in a confined, egress-locked sandbox and returns a duplex
// connection to its MCP stdio. The egress policy is deny-by-default: with no effective
// hosts the process reaches nothing; with hosts it may reach only those names and only when
// they resolve to public addresses (private, loopback, and the cloud-metadata range stay
// denied, anti-SSRF). Confinement is required, so a host that cannot enforce the read-only
// filesystem and syscall filter fails the launch. The launch command is the fixed verified
// path plus fixed args, never model-influenced.
func (l *SandboxLauncher) Launch(ctx context.Context, req LaunchRequest) (Conn, error) {
	if req.Path == "" {
		return nil, fault.New(fault.Terminal, "extension_launch_no_path", "extension: launch has no binary path")
	}

	dir := filepath.Join(l.workRoot, "ext-"+strconv.FormatInt(l.counter.Add(1), 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_launch_scratch", err)
	}

	opts := []sandbox.LocalOption{
		sandbox.WithReadOnlyFS(),
		sandbox.WithSeccomp(),
		sandbox.WithResourceLimits(l.limits),
		// The binary lives outside the scratch jail; grant read to its directory so the
		// confined launch can execute it, and nothing else on the host.
		sandbox.WithReadableDir(filepath.Dir(req.Path)),
	}
	if len(req.EgressAllow) == 0 {
		// No effective egress: deny the network wholesale. This is enforced on every
		// platform, including Windows (the AppContainer is launched without the network
		// capability), which per-host governed egress is not. A network-free extension (the
		// token, which returns unsigned transactions for core to submit) runs fully
		// contained everywhere.
		opts = append(opts, sandbox.WithNetworkDenied())
	} else {
		// Specific hosts: restrict egress to exactly them, resolved to public addresses only
		// (private, loopback, and the cloud-metadata range stay denied, anti-SSRF). This is
		// enforced by the per-child proxy on platforms that have one (Linux, macOS); where a
		// platform cannot filter egress by host (Windows), the launch is refused rather than
		// run with the network unfiltered.
		opts = append(opts, sandbox.WithEgress(netguard.Policy{
			AllowPublic: true,
			AllowHosts:  append([]string(nil), req.EgressAllow...),
		}))
	}
	loc, err := sandbox.NewLocal(dir, opts...)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fault.Wrap(fault.Terminal, "extension_launch_sandbox", err)
	}

	sess, err := loc.Session(ctx, sandbox.SessionSpec{
		Argv:    append([]string{req.Path}, req.Args...),
		Confine: true,
	})
	if err != nil {
		_ = loc.Close()
		_ = os.RemoveAll(dir)
		return nil, fault.Wrap(fault.Forbidden, "extension_launch_session", err)
	}
	return &sessionConn{loc: loc, sess: sess, dir: dir}, nil
}

// sessionConn adapts a sandbox session to the Conn the process handler consumes. Stop tears
// down the process, releases the sandbox (its egress proxy), and removes the scratch dir, so
// a stopped extension leaves neither an orphan process nor a stray directory.
type sessionConn struct {
	loc  *sandbox.Local
	sess *sandbox.Session
	dir  string
}

func (c *sessionConn) Stdin() io.WriteCloser { return c.sess.Stdin() }
func (c *sessionConn) Stdout() io.Reader     { return c.sess.Stdout() }

// Diagnostics returns what the extension process wrote to standard error, so a process that
// dies during the handshake reports why instead of surfacing as a bare EOF.
func (c *sessionConn) Diagnostics() string { return c.sess.Stderr() }

func (c *sessionConn) Stop() error {
	_ = c.sess.Stop()
	_ = c.loc.Close()
	_ = os.RemoveAll(c.dir)
	return nil
}

// DevResolver resolves a dev (locally-built, unsigned) extension binary. It is the
// authoring inner loop: point flynn at a local build and mount it exactly like a released
// one, minus the download and signature. Because it runs unsigned code, it is gated by
// Enabled: unless dev mode is explicitly turned on, a dev source is refused, and a released
// source is always refused here (the signed-distribution resolver owns that path), so this
// resolver can never run remote or unverified code in a normal run.
type DevResolver struct {
	// Enabled turns dev mode on. It must be an explicit, deliberate opt-in; the zero value
	// refuses every source so a misconfiguration fails closed.
	Enabled bool
}

// Resolve returns the local dev binary path when dev mode is enabled and the block declares
// one. A released source, a missing dev path, or dev mode off all fail closed.
func (r DevResolver) Resolve(_ context.Context, extName string, block ProcessBlock) (string, []string, error) {
	if block.Release != nil {
		return "", nil, fault.New(fault.Terminal, "extension_dev_release_unsupported",
			"extension: dev resolver cannot resolve a released source for "+extName+"; a signed-distribution resolver is required")
	}
	if !r.Enabled {
		return "", nil, fault.New(fault.Forbidden, "extension_dev_disabled",
			"extension: "+extName+" declares a dev source but dev mode is not enabled; refusing to run unsigned code")
	}
	if block.Dev == nil || block.Dev.Path == "" {
		return "", nil, fault.New(fault.Terminal, "extension_dev_no_path",
			"extension: "+extName+" has no dev binary path")
	}
	if !filepath.IsAbs(block.Dev.Path) {
		return "", nil, fault.New(fault.Terminal, "extension_dev_rel_path",
			"extension: "+extName+" dev path must be absolute")
	}
	info, err := os.Stat(block.Dev.Path)
	if err != nil {
		return "", nil, fault.Wrap(fault.Terminal, "extension_dev_stat", err)
	}
	if info.IsDir() {
		return "", nil, fault.New(fault.Terminal, "extension_dev_is_dir",
			"extension: "+extName+" dev path is a directory, not a binary")
	}
	return block.Dev.Path, block.Args, nil
}
