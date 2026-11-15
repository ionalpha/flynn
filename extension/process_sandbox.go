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

// Hard caps on one extension process, so a runaway one cannot exhaust memory
// or fork-bomb the host.
const (
	defaultExtensionMemoryMiB    = 512
	defaultExtensionMaxProcesses = 32
)

// SandboxLauncher is the production [Launcher]: a fresh kernel-confined sandbox
// with a read-only host, syscall filter, a scrubbed environment that carries no
// secret, resource caps, and egress limited to the allow-list. Where the host
// cannot confine the process, the launch is refused.
type SandboxLauncher struct {
	workRoot string
	limits   sandbox.ResourceLimits
	counter  atomic.Int64
}

// NewSandboxLauncher returns a launcher that gives each launch a fresh, empty
// scratch directory under workRoot, removed on Stop.
func NewSandboxLauncher(workRoot string) *SandboxLauncher {
	return &SandboxLauncher{
		workRoot: workRoot,
		limits:   sandbox.ResourceLimits{MemoryMiB: defaultExtensionMemoryMiB, MaxProcesses: defaultExtensionMaxProcesses},
	}
}

// Launch starts req.Path confined. With no egress hosts the process reaches
// nothing; with hosts it reaches only those names, at public addresses only,
// against SSRF. A host that cannot enforce the read-only filesystem and
// syscall filter fails the launch.
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
		// The binary lives outside the scratch jail, so its directory alone is
		// made readable.
		sandbox.WithReadableDir(filepath.Dir(req.Path)),
	}
	if len(req.EgressAllow) == 0 {
		// Denying all network works on every platform, Windows included
		// (AppContainer without the network capability).
		opts = append(opts, sandbox.WithNetworkDenied())
	} else {
		// Per-host egress needs the per-child proxy (Linux, macOS). Windows cannot
		// filter by host, so the launch is refused there.
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

// sessionConn is a [Conn] over a sandbox session. Stop also releases the
// egress proxy and removes the scratch directory.
type sessionConn struct {
	loc  *sandbox.Local
	sess *sandbox.Session
	dir  string
}

func (c *sessionConn) Stdin() io.WriteCloser { return c.sess.Stdin() }
func (c *sessionConn) Stdout() io.Reader     { return c.sess.Stdout() }

// Diagnostics returns the process's stderr.
func (c *sessionConn) Diagnostics() string { return c.sess.Stderr() }

func (c *sessionConn) Stop() error {
	_ = c.sess.Stop()
	_ = c.loc.Close()
	_ = os.RemoveAll(c.dir)
	return nil
}

// DevResolver resolves an unsigned local build for authoring. It refuses
// everything unless Enabled, and always refuses a release source, so it never
// runs remote code.
type DevResolver struct {
	// Enabled turns dev mode on; the zero value fails closed.
	Enabled bool
}

// Resolve returns the absolute dev binary path, or fails closed.
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
