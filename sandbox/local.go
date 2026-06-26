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
// outside the working tree. WithNetworkDenied, WithReadOnlyFS, and WithSeccomp add
// kernel-enforced network, filesystem, and syscall isolation where the platform
// supports it; the container, microVM, and remote tiers build on the same Sandbox
// port. Local is always present underneath them as the default-deny FS floor.
type Local struct {
	root        string // absolute, symlinks resolved
	execTimeout time.Duration
	granted     map[string]string // env vars explicitly granted into commands
	denyNetwork bool              // run commands with no network (see WithNetworkDenied)
	readonlyFS  bool              // run commands with a read-only host (see WithReadOnlyFS)
	seccomp     bool              // run commands under a syscall filter (see WithSeccomp)
	// confineBestEffort marks confinement as the always-on default (see
	// WithDefaultConfinement) rather than an explicit request, so a host that cannot
	// set it up falls back to the floor instead of failing the command.
	confineBestEffort bool
	// selfExe overrides the re-exec target used by filesystem confinement; empty means
	// the running binary (/proc/self/exe). Tests set it to a missing path to force a
	// confinement start failure and exercise the best-effort fallback deterministically.
	selfExe string
}

// LocalOption configures a Local sandbox.
type LocalOption func(*Local)

// WithExecTimeout caps how long a single command may run (0, the default, applies
// no cap beyond the caller's context). It is the integration point the
// resource-limit and circuit-breaker layer builds on.
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

// WithSeccomp runs commands under a syscall filter that refuses the calls a command
// working in its own directory has no honest need for and that would let it escalate
// privilege, escape its confinement, tamper with the kernel, or reach into other
// processes. Ordinary file, process, memory, and IO calls are left allowed, so normal
// commands run unaffected; a refused call fails with a permission error rather than
// killing the command. It is enforced by the kernel and inherited across the exec. On
// a platform that cannot provide it, a command run under this option fails rather than
// running with no syscall filter in place.
func WithSeccomp() LocalOption {
	return func(l *Local) { l.seccomp = true }
}

// WithKernelConfinement enables the full kernel-confined tier in one call: no
// network, a read-only host, and the syscall filter together. A Local built with it
// reports ContainmentKernel where the platform enforces the confinement, the level
// suitable for semi-trusted, model-authored code. It is the same as passing
// WithNetworkDenied, WithReadOnlyFS, and WithSeccomp.
func WithKernelConfinement() LocalOption {
	return func(l *Local) {
		l.denyNetwork = true
		l.readonlyFS = true
		l.seccomp = true
	}
}

// WithDefaultConfinement is the secure-by-default baseline: it enables the kernel
// confinements the current platform can enforce, and nothing where it cannot, so it
// is safe to apply unconditionally. A command is confined where the OS supports it and
// runs at the process-jail floor elsewhere, never refused merely for the default
// asking. It enables the read-only host and the syscall filter, the two confinements
// that hold for ordinary commands without breaking them: a command still reads the
// host and works in its directory, it just cannot change the host or make a dangerous
// syscall. Network denial is deliberately not part of the default, since a blanket
// no-network policy would break commands a run is legitimately allowed to make; that
// is governed per run instead. Explicit options (WithReadOnlyFS, WithSeccomp,
// WithNetworkDenied) still fail loudly on a platform that cannot honor them; this one
// does not, because it is the always-on baseline rather than an explicit request.
func WithDefaultConfinement() LocalOption {
	return func(l *Local) {
		l.confineBestEffort = true
		if kernelConfinementSupported() {
			l.readonlyFS = true
			l.seccomp = true
		}
	}
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

// Containment reports how strongly this Local confines the commands it runs. By
// default it is a process jail (ContainmentNone): it confines paths and scrubs the
// environment but shares the host kernel, network, and syscalls, so it is for trusted
// code only. When the network, filesystem, and syscall confinements are all enabled
// and the platform enforces them, it rises to ContainmentKernel: the command cannot
// reach the network, change the host filesystem, or make a dangerous syscall, which
// is the boundary for semi-trusted, model-authored code over a shared kernel. It does
// not claim ContainmentKernel where the platform cannot enforce that confinement (a
// command there is refused rather than run), so the reported level never outruns what
// actually holds.
func (l *Local) Containment() Containment {
	if l.denyNetwork && l.readonlyFS && l.seccomp && kernelConfinementSupported() {
		return ContainmentKernel
	}
	return ContainmentNone
}

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
//
// When confinement was requested as the secure-by-default baseline (see
// WithDefaultConfinement) and the kernel refuses to set it up on this host, for
// example where unprivileged user namespaces are restricted, the command is run again
// at the process-jail floor rather than failing. The default baseline is always-on,
// so it degrades to the floor it can always provide; an explicitly requested
// confinement still fails loudly, since the caller asked for it by name.
func (l *Local) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	if l.execTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.execTimeout)
		defer cancel()
	}
	confined := l.denyNetwork || l.readonlyFS || l.seccomp
	res, err := l.execOnce(ctx, cmd, confined)
	// A confined run that could not start (an error, not a non-zero exit) under the
	// always-on baseline falls back to the floor. The failed attempt never ran the
	// command, so there is nothing to undo before retrying.
	if err != nil && confined && l.confineBestEffort {
		return l.execOnce(ctx, cmd, false)
	}
	return res, err
}

// execOnce runs the command once, applying the configured confinement only when
// confined is true.
func (l *Local) execOnce(ctx context.Context, cmd Command, confined bool) (ExecResult, error) {
	name, args := shell(cmd.Line)
	//nolint:gosec // running a model-supplied command is the bash tool's purpose; isolation is this sandbox's job, hardened by the stronger tiers
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = l.root
	// Deny-by-default environment: never inherit the host's, so the agent's API
	// keys and every other process secret are withheld from a model-run command.
	// The command sees only a minimal baseline plus what WithEnv explicitly grants.
	c.Env = l.env()
	if confined {
		if err := l.confine(c); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: confine: %w", err)
		}
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
