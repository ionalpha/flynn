//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// grantRecorder replaces the propagating write with one that records the
// directories it was called for and then delegates, so a test counts propagations
// and still gets a real grant.
type grantRecorder struct {
	mu   sync.Mutex
	dirs []string
}

func (r *grantRecorder) install(t *testing.T) {
	t.Helper()
	prev := setTreeDACL
	setTreeDACL = func(ctx context.Context, dir string, acl *windows.ACL) error {
		r.mu.Lock()
		r.dirs = append(r.dirs, dir)
		r.mu.Unlock()
		return prev(ctx, dir, acl)
	}
	t.Cleanup(func() { setTreeDACL = prev })

	markers := t.TempDir()
	prevMarkers := grantMarkerDir
	grantMarkerDir = func() string { return markers }
	t.Cleanup(func() { grantMarkerDir = prevMarkers })
}

func (r *grantRecorder) count(dir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dirs {
		if strings.EqualFold(filepath.Clean(d), filepath.Clean(dir)) {
			n++
		}
	}
	return n
}

// makeTree fills dir with files spread over twenty directories, the shape that
// makes a propagating grant slow.
func makeTree(t *testing.T, dir string, files int) {
	t.Helper()
	for i := range files {
		sub := filepath.Join(dir, fmt.Sprintf("d%d", i%20))
		if err := os.MkdirAll(sub, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newGrantLocal(t *testing.T, root string, opts ...LocalOption) *Local {
	t.Helper()
	l, err := NewLocal(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func sidFor(t *testing.T, l *Local) *windows.SID {
	t.Helper()
	sid, err := workspaceRestrictSID(l.root)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// TestEnsureGrantSkipsAGrantAlreadyInPlace covers the ways a grant is repeated:
// the next command of one Local, and the next Local on the same directory (a
// session builds one per turn).
func TestEnsureGrantSkipsAGrantAlreadyInPlace(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	first := newGrantLocal(t, t.TempDir())
	makeTree(t, first.root, 200)
	sid := sidFor(t, first)
	ctx := context.Background()

	for range 3 {
		if err := first.grantDir(ctx, first.root, sid); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.count(first.root); got != 1 {
		t.Fatalf("three grants on one Local propagated %d times, want 1", got)
	}

	second := newGrantLocal(t, first.root)
	if err := second.grantDir(ctx, first.root, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(first.root); got != 1 {
		t.Fatalf("a new Local on a granted directory propagated again (%d calls), want 1", got)
	}
}

// TestCloseKeepsTheWriteRestrictedRootGrant guards the guarantee closePlatform
// documents: closing does not revoke the working-directory grant, because
// revoking rewrites every file under it.
func TestCloseKeepsTheWriteRestrictedRootGrant(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir(), WithHostReadable())
	if err := l.grantRestrictedDir(context.Background(), l.root); err != nil {
		t.Fatal(err)
	}
	before := rec.count(l.root)
	if before != 1 {
		t.Fatalf("the first grant propagated %d times, want 1", before)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	acl, err := readDACL(l.root)
	if err != nil {
		t.Fatal(err)
	}
	if !aclHasGrant(acl, sidFor(t, l), fileAllAccess) {
		t.Fatal("Close revoked the working-directory grant")
	}

	again := newGrantLocal(t, l.root, WithHostReadable())
	if err := again.grantRestrictedDir(context.Background(), again.root); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(l.root); got != before {
		t.Fatalf("a Local after Close propagated over the root again (%d calls, was %d)", got, before)
	}
}

// TestGrantWithoutMarkerIsRedone shows why the access list alone is not trusted:
// a grant cut short leaves the entry on the directory and no marker, and the next
// launch must finish it.
func TestGrantWithoutMarkerIsRedone(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	first := newGrantLocal(t, t.TempDir())
	sid := sidFor(t, first)
	ctx := context.Background()
	if err := first.grantDir(ctx, first.root, sid); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(grantMarkerPath(first.root, sid, fileAllAccess)); err != nil {
		t.Fatal(err)
	}
	if err := newGrantLocal(t, first.root).grantDir(ctx, first.root, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(first.root); got != 2 {
		t.Fatalf("an unmarked grant propagated %d times, want 2", got)
	}
}

// TestGrantEntryRemovedIsRedone covers a marker that outlives the entry, as when
// the directory's access list is reset.
func TestGrantEntryRemovedIsRedone(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	first := newGrantLocal(t, t.TempDir())
	sid := sidFor(t, first)
	ctx := context.Background()
	if err := first.grantDir(ctx, first.root, sid); err != nil {
		t.Fatal(err)
	}
	if err := revokeDirAccess(first.root, sid); err != nil {
		t.Fatal(err)
	}
	if err := newGrantLocal(t, first.root).grantDir(ctx, first.root, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(first.root); got != 2 {
		t.Fatalf("a revoked grant propagated %d times, want 2", got)
	}
}

// TestCancelledGrantReturnsAndIsNotTrusted cancels a grant after it has rewritten
// some files. It must return the cancellation, stop short of the tree's end, and
// leave no marker, so the next launch completes it.
func TestCancelledGrantReturnsAndIsNotTrusted(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	prevEvery := grantProgressEvery
	grantProgressEvery = 1
	t.Cleanup(func() { grantProgressEvery = prevEvery })

	l := newGrantLocal(t, t.TempDir())
	const files = 3000
	makeTree(t, l.root, files)
	sid := sidFor(t, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen int64
	ctx = WithGrantProgress(ctx, func(p GrantProgress) {
		seen = max(seen, p.Files)
		if p.Files >= 20 {
			cancel()
		}
	})
	err := l.grantDir(ctx, l.root, sid)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled grant returned %v, want context.Canceled", err)
	}
	if seen >= files {
		t.Fatalf("the walk visited %d objects after cancellation, want it to stop early", seen)
	}
	if grantMarked(l.root, sid, fileAllAccess) {
		t.Fatal("a cancelled grant left a completion marker")
	}

	if err := newGrantLocal(t, l.root).grantDir(context.Background(), l.root, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(l.root); got != 2 {
		t.Fatalf("the grant after a cancelled one propagated %d times in total, want 2", got)
	}
}

// TestPreCancelledGrantReturnsPromptly checks a context cancelled before the
// grant starts is honoured without a marker.
func TestPreCancelledGrantReturnsPromptly(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	makeTree(t, l.root, 100)
	sid := sidFor(t, l)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.grantDir(ctx, l.root, sid); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant under a cancelled context returned %v, want context.Canceled", err)
	}
	if grantMarked(l.root, sid, fileAllAccess) {
		t.Fatal("a cancelled grant left a completion marker")
	}
}

// TestGrantReportsProgress checks the status a host shows instead of a hang.
func TestGrantReportsProgress(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	prevEvery := grantProgressEvery
	grantProgressEvery = 1
	t.Cleanup(func() { grantProgressEvery = prevEvery })

	l := newGrantLocal(t, t.TempDir())
	makeTree(t, l.root, 100)
	var reports []GrantProgress
	ctx := WithGrantProgress(context.Background(), func(p GrantProgress) { reports = append(reports, p) })
	if err := l.grantDir(ctx, l.root, sidFor(t, l)); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 3 {
		t.Fatalf("got %d reports, want a start, progress and an end: %+v", len(reports), reports)
	}
	if first := reports[0]; first.Dir != l.root || first.Files != 0 || first.Done {
		t.Fatalf("first report = %+v, want the directory with no files yet", first)
	}
	if last := reports[len(reports)-1]; !last.Done {
		t.Fatalf("last report = %+v, want Done", last)
	}
}

// TestProbeDoesNotGrantTheRoot proves the confinement probe, which runs before the
// model is called, leaves the working directory alone, and that commands and a
// later Local on the same root then grant it exactly once between them.
func TestProbeDoesNotGrantTheRoot(t *testing.T) {
	for name, opts := range map[string][]LocalOption{
		"appcontainer":     {WithKernelConfinement()},
		"write-restricted": {WithReadOnlyFS(), WithSeccomp(), WithHostReadable()},
	} {
		t.Run(name, func(t *testing.T) {
			var rec grantRecorder
			rec.install(t)
			ctx := context.Background()

			first := newGrantLocal(t, t.TempDir(), opts...)
			if got := first.Containment(); got != ContainmentKernel {
				t.Skipf("containment = %s on this machine, so there is no probe to observe", got)
			}
			if got := rec.count(first.root); got != 0 {
				t.Fatalf("the probe propagated over the root %d times", got)
			}
			for range 2 {
				if _, err := first.Exec(ctx, Command{Line: "echo hi"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}

			second := newGrantLocal(t, first.root, opts...)
			if _, err := second.Exec(ctx, Command{Line: "echo hi"}); err != nil {
				t.Fatal(err)
			}
			if got := rec.count(first.root); got != 1 {
				t.Fatalf("two Locals and three commands propagated over the root %d times, want 1", got)
			}
		})
	}
}
