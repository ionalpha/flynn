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
	"strings"
	"time"
)

// Local is the in-process sandbox tier: the floor that runs everywhere with no
// host support required. It confines all file operations to a working-directory
// root (a path that escapes via "..", an absolute path, or a symlink pointing
// outside is denied) and runs commands in that directory. It is not strong
// isolation on its own - a command it runs has the host user's privileges outside
// the working tree. The container, microVM, and remote tiers implement the same
// Sandbox port for that; Landlock/seccomp/cgroups hardening of this tier is a
// follow-up. Local is always present underneath them as the default-deny FS floor.
type Local struct {
	root        string // absolute, symlinks resolved
	execTimeout time.Duration
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

// shell returns the per-OS command that runs a shell command string: a POSIX shell
// everywhere except Windows, where cmd.exe is the only one guaranteed present.
func shell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}
