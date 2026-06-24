package sandbox

import (
	"context"
	"path"
	"path/filepath"
	"strings"
)

// Transport is the minimal set of primitives a remote sandbox backend provides:
// run a command, move a file in or out, list files, and tear the sandbox down. It
// is the Flynn-owned seam each cloud backend (E2B, Daytona, Modal, ...) implements
// over its own API or CLI, so a backend writes only this surface and inherits the
// full Sandbox port through Remote. Paths are relative to the remote sandbox root;
// the backend confines them server-side, and Remote adds a default-deny check on
// top so confinement never depends on the backend alone.
type Transport interface {
	// Exec runs a shell command in the remote working directory. A non-zero exit is
	// a result (in ExecResult.ExitCode), not an error; an error means the command
	// could not be run.
	Exec(ctx context.Context, line string) (ExecResult, error)
	// ReadFile reads a file from the remote working directory.
	ReadFile(ctx context.Context, p string) ([]byte, error)
	// WriteFile writes a file in the remote working directory, creating parents.
	WriteFile(ctx context.Context, p string, data []byte) error
	// List returns the regular-file paths under dir (recursive), relative to the
	// remote root. It is the primitive Glob and Walk build on, so a backend exposes
	// one listing call rather than two.
	List(ctx context.Context, dir string) ([]string, error)
	// Close tears down the remote sandbox and releases its resources.
	Close(ctx context.Context) error
}

// Remote is the remote sandbox tier: it implements the Sandbox port by mapping
// each operation onto a Transport, so isolation runs server-side in a cloud
// microVM while the host only orchestrates. It is platform-agnostic (the host
// never executes the work) and the same on Linux, macOS, and Windows. Path
// confinement is enforced here as defense-in-depth before any call reaches the
// backend, matching the Local tier's default-deny boundary.
type Remote struct {
	t Transport
}

// NewRemote builds a Remote sandbox over a backend transport.
func NewRemote(t Transport) *Remote { return &Remote{t: t} }

var _ Sandbox = (*Remote)(nil)

// Exec runs a command in the remote sandbox.
func (r *Remote) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	return r.t.Exec(ctx, cmd.Line)
}

// ReadFile reads a confined file from the remote sandbox.
func (r *Remote) ReadFile(ctx context.Context, p string) ([]byte, error) {
	c, err := confine(p)
	if err != nil {
		return nil, err
	}
	return r.t.ReadFile(ctx, c)
}

// WriteFile writes a confined file in the remote sandbox.
func (r *Remote) WriteFile(ctx context.Context, p string, data []byte) error {
	c, err := confine(p)
	if err != nil {
		return err
	}
	return r.t.WriteFile(ctx, c, data)
}

// Glob lists remote paths matching a shell pattern, relative to the root. The
// pattern is matched against each listed path with path.Match (no "**"), so it
// behaves consistently across backends rather than depending on a remote shell.
func (r *Remote) Glob(ctx context.Context, pattern string) ([]string, error) {
	entries, err := r.t.List(ctx, ".")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if ok, err := path.Match(pattern, e); err == nil && ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// Walk returns the regular-file paths under a confined root, relative to the
// sandbox root.
func (r *Remote) Walk(ctx context.Context, root string) ([]string, error) {
	c, err := confine(root)
	if err != nil {
		return nil, err
	}
	return r.t.List(ctx, c)
}

// Close tears down the remote sandbox.
func (r *Remote) Close() error { return r.t.Close(context.Background()) }

// confine validates that a caller-supplied path stays within the sandbox root and
// returns it cleaned, in slash form. An absolute path or one that escapes the root
// via ".." is denied (default-deny), the same boundary the Local tier enforces, so
// the guarantee holds even if a backend's own confinement is weaker. An empty path
// is the root itself (".").
func confine(p string) (string, error) {
	clean := path.Clean(filepath.ToSlash(p))
	if clean == "" || clean == "." {
		return ".", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrDenied
	}
	return clean, nil
}
