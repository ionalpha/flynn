package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeOutsideSecret creates a secret file just outside the sandbox root and returns its
// path, registering cleanup. It is the out-of-root target a symlink escape would try to
// reach.
func writeOutsideSecret(t *testing.T, l *Local) string {
	t.Helper()
	secret := filepath.Join(filepath.Dir(l.root), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })
	return secret
}

// A symlink occupying a target path inside the root, but pointing outside it, must not be
// opened through, even when the lexical path is within the root (the state resolve sees
// just before a swap). This is the core of the fix: confinedOpen resolves and opens the
// same object and refuses one that leaves the root.
func TestConfinedOpenRefusesEscapingSymlink(t *testing.T) {
	l := newLocal(t)
	secret := writeOutsideSecret(t, l)

	abs := filepath.Join(l.root, "x")
	if err := os.Symlink(secret, abs); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := l.confinedOpen(abs, os.O_RDONLY, 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("read through escaping symlink: err=%v, want ErrDenied", err)
	}
	if _, err := l.confinedOpen(abs, os.O_WRONLY|os.O_TRUNC, 0o644); !errors.Is(err, ErrDenied) {
		t.Fatalf("write through escaping symlink: err=%v, want ErrDenied", err)
	}

	// The secret must be untouched: nothing was written through the link.
	data, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte("SECRET")) {
		t.Fatalf("secret was modified through the symlink: %q", data)
	}
}

// A normal regular file inside the root opens and round-trips, so the symlink-safe open
// did not break ordinary confined IO.
func TestConfinedOpenAllowsRegularFile(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	if err := l.WriteFile(ctx, "dir/file.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := l.ReadFile(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("round-trip = %q, want %q", got, "hello")
	}
}

// The time-of-check/time-of-use race itself: while a reader repeatedly reads a path, a
// writer repeatedly swaps that path between a safe in-root file and a symlink pointing at
// an out-of-root secret. With the symlink-safe open, no read may ever return the secret;
// either the read is denied or it returns the in-root content, never the escape. Without
// the fix, the window between resolve and the open lets a read follow the swapped link out
// and surface the secret.
func TestConfinedOpenSymlinkSwapRaceNeverLeaks(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	secret := writeOutsideSecret(t, l)

	target := filepath.Join(l.root, "x")
	if err := os.Symlink(secret, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_ = os.Remove(target)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(target)
			_ = os.WriteFile(target, []byte("safe"), 0o644)
			_ = os.Remove(target)
			_ = os.Symlink(secret, target)
		}
	}()

	for range 3000 {
		data, err := l.ReadFile(ctx, "x")
		if err == nil && bytes.Contains(data, []byte("SECRET")) {
			close(stop)
			<-done
			t.Fatal("TOCTOU: a read followed a swapped symlink and leaked the out-of-root secret")
		}
	}
	close(stop)
	<-done
}

// A parent directory swapped to a symlink pointing out of the root after resolve ran must
// not have directories created through it. resolve would deny the path if it saw the
// symlink, so this calls mkdirParents directly with the state a swap leaves behind.
func TestMkdirParentsRefusesEscapingSymlinkedParent(t *testing.T) {
	l := newLocal(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(l.root, "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := l.mkdirParents(filepath.Join(l.root, "out", "made", "f.txt")); err == nil {
		t.Fatal("mkdirParents created directories through a symlink that leaves the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "made")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a directory was created outside the root: %v", err)
	}
}

func TestMkdirParentsCreatesNestedDirectories(t *testing.T) {
	l := newLocal(t)
	if err := l.mkdirParents(filepath.Join(l.root, "a", "b", "f.txt")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(l.root, "a", "b")); err != nil || !fi.IsDir() {
		t.Fatalf("parents not created: %v", err)
	}
	// A file directly in the root has no parent to create.
	if err := l.mkdirParents(filepath.Join(l.root, "f.txt")); err != nil {
		t.Fatalf("a file in the root must need no parents: %v", err)
	}
}

func TestConfinedRelRefusesPathsOutsideTheRoot(t *testing.T) {
	l := newLocal(t)
	for _, abs := range []string{filepath.Dir(l.root), filepath.Join(l.root, "..", "x")} {
		if rel, err := l.confinedRel(abs); !errors.Is(err, ErrDenied) {
			t.Fatalf("confinedRel(%q) = %q, %v; want ErrDenied", abs, rel, err)
		}
	}
}
