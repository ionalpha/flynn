package sandbox

import (
	"context"
	"path"
	"path/filepath"
	"strings"
)

// Transport is what a cloud backend (E2B, Daytona, Modal) implements to get the
// full Sandbox port through Remote. Paths are relative to the remote root. Remote
// checks them before the backend does, so confinement never rests on the backend
// alone.
type Transport interface {
	// Exec reports a non-zero exit in ExecResult.ExitCode; an error means the
	// command did not run.
	Exec(ctx context.Context, line string) (ExecResult, error)
	// ReadFile takes a path Remote has already confined.
	ReadFile(ctx context.Context, p string) ([]byte, error)
	// WriteFile creates parent directories.
	WriteFile(ctx context.Context, p string, data []byte) error
	// List returns regular files under dir, recursively, relative to the remote
	// root. Glob and Walk both build on it.
	List(ctx context.Context, dir string) ([]string, error)
	// Close tears down the remote sandbox.
	Close(ctx context.Context) error
}

// Remote runs the work server-side over a Transport, so it behaves the same on
// every host platform. It confines paths before any call reaches the backend.
type Remote struct {
	t Transport
}

// NewRemote builds a Remote sandbox over a backend transport.
func NewRemote(t Transport) *Remote { return &Remote{t: t} }

var _ Sandbox = (*Remote)(nil)

// Exec sends only Command.Line; the other fields are ignored.
func (r *Remote) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	return r.t.Exec(ctx, cmd.Line)
}

// ReadFile returns [ErrDenied] for a path outside the root.
func (r *Remote) ReadFile(ctx context.Context, p string) ([]byte, error) {
	c, err := confine(p)
	if err != nil {
		return nil, err
	}
	return r.t.ReadFile(ctx, c)
}

// WriteFile returns [ErrDenied] for a path outside the root.
func (r *Remote) WriteFile(ctx context.Context, p string, data []byte) error {
	c, err := confine(p)
	if err != nil {
		return err
	}
	return r.t.WriteFile(ctx, c, data)
}

// Glob matches with path.Match (no "**") on every listed path, so the result does
// not depend on a backend's shell.
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

// Walk returns [ErrDenied] for a root outside the sandbox.
func (r *Remote) Walk(ctx context.Context, root string) ([]string, error) {
	c, err := confine(root)
	if err != nil {
		return nil, err
	}
	return r.t.List(ctx, c)
}

// Close tears down the remote sandbox with a background context.
func (r *Remote) Close() error { return r.t.Close(context.Background()) }

// confine returns p cleaned in slash form, denying an absolute path or a ".."
// escape so the guarantee holds over a weaker backend. Empty is the root.
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
