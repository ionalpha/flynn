package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// externalWritable creates a directory outside the sandbox workspace and returns it with
// the path of a file a confined child will try to create inside it, standing in for the
// credential home an external CLI keeps its own mutable state in.
func externalWritable(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "state.txt")
}

// writeAttempt runs the helper under the given sandbox, asking it to write target, and
// returns its report ("wrote" or "writeerr").
func writeAttempt(t *testing.T, l *Local, target string) string {
	t.Helper()
	bin := copyHelperInto(t, l.Root())
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_WRITEFILE=" + target},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return res.Output
}

// requireConfinedRun skips unless this host can actually establish kernel confinement for
// l. Whether a platform has a confinement mechanism (kernelConfinementSupported) is a
// compile-time fact; whether this host will let an unprivileged process use it is not.
// Ubuntu 24.04 and its GitHub runner restrict unprivileged user namespaces, and the
// always-on baseline (WithDefaultConfinement) is documented to fall back to the floor on a
// host that cannot set confinement up. On such a host an ungranted write is not denied and
// a granted one proves nothing, because the child ran unconfined. Both directions have to
// gate on the runtime probe rather than the platform predicate: otherwise the denial test
// fails for the wrong reason and the grant test passes for the wrong reason.
func requireConfinedRun(t *testing.T, l *Local) {
	t.Helper()
	if !l.kernelConfinementEnforceable() {
		t.Skip("this host cannot establish kernel confinement (unprivileged user namespaces are likely restricted); the always-on baseline runs at the floor, where a write outside the workspace is not denied")
	}
}

func TestWritableDirGrantsConfinedWrite(t *testing.T) {
	ext, target := externalWritable(t)
	l := newTestLocal(t, WithDefaultConfinement(), WithWritableDir(ext))
	requireConfinedRun(t, l)
	if out := writeAttempt(t, l, target); !strings.Contains(out, "wrote") {
		t.Fatalf("confined child could not write the granted external dir: %q", out)
	}
	// The grant is what let the write land, so the file must actually exist on the host:
	// a child reporting success into a private overlay would be a confinement that only
	// looks like it granted the directory.
	if b, err := os.ReadFile(target); err != nil || string(b) != "written" {
		t.Fatalf("granted write did not reach the host directory: %q err=%v", b, err)
	}
}

func TestWritableDirDeniedWithoutGrant(t *testing.T) {
	_, target := externalWritable(t)
	l := newTestLocal(t, WithDefaultConfinement()) // no WithWritableDir
	requireConfinedRun(t, l)
	if out := writeAttempt(t, l, target); strings.Contains(out, "wrote") {
		t.Fatalf("confined child wrote an ungranted external dir; the confinement is not denying writes: %q", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("an ungranted external file was created at %s", target)
	}
}

func TestWritableDirRevokedOnClose(t *testing.T) {
	if runtime.GOOS != "windows" {
		// Off Windows the grant lives in the child's own confinement (a bind mount, a
		// profile rule) and dies with the process, so there is no host access entry that
		// could outlive Close and nothing for this test to catch.
		t.Skip("the write grant is a revocable host access entry on Windows only")
	}
	root := t.TempDir()
	ext, target := externalWritable(t)

	// Both sandboxes are rooted at the same directory, so they derive the same confined
	// identity. The first grants the external directory and Closes, which must revoke;
	// the second, granting nothing, must then be denied. A grant that survived Close
	// would let the second child write too.
	l1, err := NewLocal(root, WithDefaultConfinement(), WithWritableDir(ext))
	if err != nil {
		t.Fatalf("NewLocal l1: %v", err)
	}
	if out := writeAttempt(t, l1, target); !strings.Contains(out, "wrote") {
		t.Fatalf("granted write failed: %q", out)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close l1: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}

	l2, err := NewLocal(root, WithDefaultConfinement())
	if err != nil {
		t.Fatalf("NewLocal l2: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	if out := writeAttempt(t, l2, target); strings.Contains(out, "wrote") {
		t.Fatalf("the write grant survived Close; revoke did not run: %q", out)
	}
}

// TestWritableDirGrantsWriteRestrictedTier covers the second Windows tier, which gates
// writes on a restricting SID rather than on a container identity: the grant has to be
// applied to that SID instead, and a tier that quietly skipped it would leave the codex
// episode unable to write its own credential home.
func TestWritableDirGrantsWriteRestrictedTier(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the write-restricted tier is Windows-only")
	}
	ext, target := externalWritable(t)
	l := newTestLocal(t, WithDefaultConfinement(), WithHostReadable(), WithWritableDir(ext))
	if out := writeAttempt(t, l, target); !strings.Contains(out, "wrote") {
		t.Fatalf("write-restricted child could not write the granted external dir: %q", out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("granted write did not reach the host directory: %v", err)
	}
}

// TestWriteRestrictedDeniesUngrantedDir is the negative half of the tier above: the
// weaker read posture must not come with a weaker write posture.
func TestWriteRestrictedDeniesUngrantedDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the write-restricted tier is Windows-only")
	}
	_, target := externalWritable(t)
	l := newTestLocal(t, WithDefaultConfinement(), WithHostReadable())
	if out := writeAttempt(t, l, target); strings.Contains(out, "wrote") {
		t.Fatalf("write-restricted child wrote an ungranted external dir: %q", out)
	}
}
