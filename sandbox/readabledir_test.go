package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExternalSecret creates a directory outside the sandbox workspace holding a file,
// standing in for an external CLI's auth or config home, and returns the directory and
// the file path.
func writeExternalSecret(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "auth.txt")
	if err := os.WriteFile(file, []byte("token123"), 0o600); err != nil {
		t.Fatalf("write external secret: %v", err)
	}
	return dir, file
}

func TestReadableDirGrantsConfinedRead(t *testing.T) {
	ext, secret := writeExternalSecret(t)
	l := newTestLocal(t, WithDefaultConfinement(), WithReadableDir(ext))
	// The confined child can exec only a binary it can read: copy the helper into the
	// workspace, the one location the confinement grants it outright.
	bin := copyHelperInto(t, l.Root())
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_READFILE=" + secret},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(res.Output, "read:token123") {
		t.Fatalf("confined child could not read the granted external dir: %q", res.Output)
	}
}

func TestReadableDirDeniedWithoutGrant(t *testing.T) {
	if runtime.GOOS != "windows" {
		// Only the Windows AppContainer denies reads by default; a read-only host on the
		// other platforms permits the read even without the grant, so there is nothing to
		// deny and the negative case does not apply.
		t.Skip("read-deny-by-default confinement is Windows-only")
	}
	_, secret := writeExternalSecret(t)
	l := newTestLocal(t, WithDefaultConfinement()) // no WithReadableDir
	bin := copyHelperInto(t, l.Root())
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_READFILE=" + secret},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if strings.Contains(res.Output, "read:token123") {
		t.Fatalf("confined child read an ungranted external dir; the container is not denying reads: %q", res.Output)
	}
}

func TestReadableDirRevokedOnClose(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The grant, and so the revoke, is a no-op where a read-only host already permits
		// the read. This behavioral revoke check applies to the Windows AppContainer only.
		t.Skip("read grant/revoke is Windows-only")
	}
	root := t.TempDir()
	ext, secret := writeExternalSecret(t)

	// The container SID is derived from the workspace path, so two sandboxes rooted at the
	// same directory share one SID. The first grants the external dir and Closes (which
	// must revoke); the second, rooted identically but granting nothing, must then be
	// denied. If the grant survived Close, the second would still read the file.
	l1, err := NewLocal(root, WithDefaultConfinement(), WithReadableDir(ext))
	if err != nil {
		t.Fatalf("NewLocal l1: %v", err)
	}
	bin := copyHelperInto(t, l1.Root())
	env := []string{"SANDBOX_STREAM_HELPER=1", "HELPER_READFILE=" + secret}
	if _, err := l1.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  env,
	}); err != nil {
		t.Fatalf("Capture l1: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close l1: %v", err)
	}

	l2, err := NewLocal(root, WithDefaultConfinement())
	if err != nil {
		t.Fatalf("NewLocal l2: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	res, err := l2.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  env,
	})
	if err != nil {
		t.Fatalf("Capture l2: %v", err)
	}
	if strings.Contains(res.Output, "read:token123") {
		t.Fatalf("the read grant survived Close; revoke did not run: %q", res.Output)
	}
}
