package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func newLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLocalReadWrite(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	if err := l.WriteFile(ctx, "sub/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	b, err := l.ReadFile(ctx, "sub/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("read back %q", b)
	}
}

func TestLocalExec(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	res, err := l.Exec(ctx, Command{Line: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "hello") || res.ExitCode != 0 {
		t.Fatalf("echo = %+v", res)
	}
	// A non-zero exit is a result, not an error.
	res, err = l.Exec(ctx, Command{Line: "exit 7"})
	if err != nil {
		t.Fatalf("non-zero exit should not error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}
}

func TestLocalGlobAndWalk(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	for _, f := range []string{"a.go", "b.go", "sub/c.go", "sub/d.txt"} {
		if err := l.WriteFile(ctx, f, nil); err != nil {
			t.Fatal(err)
		}
	}
	g, err := l.Glob(ctx, "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 2 {
		t.Fatalf("glob *.go = %v", g)
	}
	w, err := l.Walk(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 4 {
		t.Fatalf("walk = %v (want 4 files)", w)
	}
	for _, p := range w {
		if filepath.IsAbs(p) {
			t.Fatalf("walk returned an absolute path: %q", p)
		}
	}
}

func TestLocalConfinementRejectsEscape(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(l.root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(outside) }()

	for _, p := range []string{"../secret.txt", "../../secret.txt", "sub/../../secret.txt"} {
		if _, err := l.ReadFile(ctx, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("read %q: err=%v, want ErrDenied", p, err)
		}
		if err := l.WriteFile(ctx, p, []byte("x")); !errors.Is(err, ErrDenied) {
			t.Fatalf("write %q: err=%v, want ErrDenied", p, err)
		}
	}
}

func TestLocalSymlinkEscapeDenied(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	target := filepath.Join(filepath.Dir(l.root), "outside.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(target) }()
	if err := os.Symlink(target, filepath.Join(l.root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := l.ReadFile(ctx, "link.txt"); !errors.Is(err, ErrDenied) {
		t.Fatalf("symlink escape: err=%v, want ErrDenied", err)
	}
}

// TestConfinementProperty is the boundary invariant: for any caller-supplied path
// (relative, absolute, or studded with ".."), resolve either denies it or returns
// a path within the root. It never yields one that escapes.
func TestConfinementProperty(t *testing.T) {
	l := newLocal(t)
	seg := rapid.SampledFrom([]string{"..", "a", "b", "sub", "x.txt", ".", "deep"})
	rapid.Check(t, func(rt *rapid.T) {
		parts := rapid.SliceOfN(seg, 1, 6).Draw(rt, "parts")
		p := strings.Join(parts, "/")
		if rapid.Bool().Draw(rt, "absolute") {
			p = "/" + p
		}
		abs, err := l.resolve(p)
		if err != nil {
			return // denied: safe
		}
		if !l.within(abs) {
			rt.Fatalf("resolve(%q) escaped the root: %q (root %q)", p, abs, l.root)
		}
	})
}
