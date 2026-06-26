package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Local is the in-process sandbox tier: the floor that runs everywhere with no
// host support required. It confines all file operations to a working-directory
// root (a path that escapes via "..", an absolute path, or a symlink pointing
// outside is denied) and runs commands in that directory. It is not strong
// isolation on its own - a command it runs otherwise has the host user's privileges
// outside the working tree. WithNetworkDenied and WithReadOnlyFS add kernel-enforced
// network and filesystem isolation where the platform supports it; further syscall
// confinement, and the container, microVM, and remote tiers, build on the same
// Sandbox port. Local is always present underneath them as the default-deny FS floor.
type Local struct {
	root        string // absolute, symlinks resolved
	execTimeout time.Duration
	granted     map[string]string // env vars explicitly granted into commands
	denyNetwork bool              // run commands with no network (see WithNetworkDenied)
	readonlyFS  bool              // run commands with a read-only host (see WithReadOnlyFS)
}

// LocalOption configures a Local sandbox.
type LocalOption func(*Local)

// WithExecTimeout caps how long a single command may run (0, the default, applies
// no cap beyond the caller's context). It is the seam the resource-limit and
// circuit-breaker layer builds on.
func WithExecTimeout(d time.Duration) LocalOption {
	return func(l *Local) {
		if d > 0 {
			l.execTimeout = d
		}
	}
}

// WithEnv grants additional environment variables into commands the sandbox runs.
// A command's environment is otherwise a minimal, secret-free baseline (see Exec):
// the host's environment is never inherited, so the agent's own credentials are
// withheld by construction. This is the only way a variable beyond the baseline
// reaches a command, the brokered path a capability grant feeds. A granted value
// overrides the baseline value for the same key. Secrets must not be granted here;
// they belong in a request the sandbox never sees, not a child's environment.
func WithEnv(vars map[string]string) LocalOption {
	return func(l *Local) {
		if len(vars) == 0 {
			return
		}
		if l.granted == nil {
			l.granted = make(map[string]string, len(vars))
		}
		for k, v := range vars {
			l.granted[k] = v
		}
	}
}

// WithNetworkDenied runs commands with no network access, the OS counterpart of the
// in-process egress policy: a command the sandbox runs cannot reach out (no exfil, no
// command-and-control), which matters most for code we do not fully trust. It is
// enforced by the kernel, by running the command in a network namespace with no
// interfaces, so the command sees only a down loopback and no routes. On a platform
// or host that cannot provide it, a command run under this option fails rather than
// running with the network silently still open.
func WithNetworkDenied() LocalOption {
	return func(l *Local) { l.denyNetwork = true }
}

// WithReadOnlyFS runs commands against a read-only view of the host: the command can
// read what the host user can, but the only thing it can write is its own working
// directory (and a private scratch area). A command we do not fully trust therefore
// cannot tamper with host files, plant a persistent change outside the working tree,
// or modify another project. It is enforced by the kernel, by running the command in
// a mount namespace where every host mount is read-only and only the working tree is
// remounted writable. On a platform that cannot provide it, a command run under this
// option fails rather than running with the host filesystem silently still writable.
func WithReadOnlyFS() LocalOption {
	return func(l *Local) { l.readonlyFS = true }
}

// NewLocal builds a Local sandbox rooted at dir. The root is resolved to an
// absolute, symlink-free path once, so confinement checks compare against a stable
// base.
func NewLocal(dir string, opts ...LocalOption) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	l := &Local{root: abs}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

var _ Sandbox = (*Local)(nil)

// Root returns the absolute working directory the sandbox is confined to.
func (l *Local) Root() string { return l.root }

// Close releases resources (none for the local tier).
func (l *Local) Close() error { return nil }

// Containment reports that the local tier is a process jail only: it confines paths
// and scrubs the environment but shares the host kernel, network, and syscalls, so it
// is trusted-code-only. Untrusted work is refused here until a stronger tier runs it.
func (l *Local) Containment() Containment { return ContainmentNone }

// resolve confines a caller-supplied path to the root and returns the absolute
// path to operate on, or ErrDenied. The nearest existing ancestor is
// symlink-resolved and re-checked so a symlink cannot point the operation outside.
func (l *Local) resolve(p string) (string, error) {
	abs := filepath.Clean(filepath.Join(l.root, p))
	if !l.within(abs) {
		return "", ErrDenied
	}
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !l.within(resolved) {
				return "", ErrDenied
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return abs, nil
}

func (l *Local) within(p string) bool {
	if p == l.root {
		return true
	}
	rel, err := filepath.Rel(l.root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rel renders an absolute path back as root-relative for callers, so they never
// see absolute host paths.
func (l *Local) rel(abs string) string {
	if r, err := filepath.Rel(l.root, abs); err == nil {
		return filepath.ToSlash(r)
	}
	return abs
}

// Exec runs a command through a per-OS shell in the working directory. A non-zero
// exit is returned as a result, not an error; only a failure to start (or a
// cancelled context) is an error.
func (l *Local) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	if l.execTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.execTimeout)
		defer cancel()
	}
	name, args := shell(cmd.Line)
	//nolint:gosec // running a model-supplied command is the bash tool's purpose; isolation is this sandbox's job, hardened by the stronger tiers
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = l.root
	// Deny-by-default environment: never inherit the host's, so the agent's API
	// keys and every other process secret are withheld from a model-run command.
	// The command sees only a minimal baseline plus what WithEnv explicitly grants.
	c.Env = l.env()
	if err := l.confine(c); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: confine: %w", err)
	}
	out, err := c.CombinedOutput()
	res := ExecResult{Output: string(out)}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("sandbox: exec: %w", err)
	}
	return res, nil
}

// ReadFile reads a confined file.
func (l *Local) ReadFile(_ context.Context, path string) ([]byte, error) {
	abs, err := l.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs) //nolint:gosec // abs is confined to the sandbox root by resolve
}

// WriteFile writes a confined file, creating parent directories.
func (l *Local) WriteFile(_ context.Context, path string, data []byte) error {
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644) //nolint:gosec // abs is confined to the sandbox root; 0644 is intended for agent-written files
}

// Glob lists confined paths matching a shell pattern, relative to the root.
func (l *Local) Glob(_ context.Context, pattern string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(l.root, pattern))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if l.within(m) {
			out = append(out, l.rel(m))
		}
	}
	return out, nil
}

// Walk returns the regular-file paths under root, relative to the sandbox root.
func (l *Local) Walk(_ context.Context, root string) ([]string, error) {
	base, err := l.resolve(root)
	if err != nil {
		return nil, err
	}
	var out []string
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		out = append(out, l.rel(p))
		return nil
	})
	return out, walkErr
}

// env builds the environment a command runs with: the values of the baseline keys
// that are set on the host, plus any explicitly granted variables (which override
// a baseline key of the same name). The host environment is never passed through,
// so no credential the agent holds can reach a command unless it was granted by
// name. The result is sorted for a stable, testable ordering.
func (l *Local) env() []string {
	vals := make(map[string]string, len(baselineEnvKeys)+len(l.granted))
	for _, k := range baselineEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			vals[k] = v
		}
	}
	for k, v := range l.granted {
		vals[k] = v
	}
	out := make([]string, 0, len(vals))
	for k, v := range vals {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// baselineEnvKeys are the only host environment variables a sandboxed command
// inherits: just enough for a shell to start and find tools, a home, and a temp
// directory. The list is deliberately tiny and holds no credential-bearing names;
// anything else a command needs must be granted explicitly with WithEnv. It is
// platform-split because the two shells need different essentials (cmd.exe will not
// start without SystemRoot).
var baselineEnvKeys = baselineKeys()

func baselineKeys() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"SystemRoot", "windir", "ComSpec", "PATHEXT", "PATH",
			"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
			"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		}
	}
	return []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}
}

// shell returns the per-OS command that runs a shell command string: a POSIX shell
// everywhere except Windows, where cmd.exe is the only one guaranteed present.
func shell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}
