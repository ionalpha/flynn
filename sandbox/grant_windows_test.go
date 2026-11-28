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
	"time"

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
	setTreeDACL = func(dir string, acl *windows.ACL) error {
		r.mu.Lock()
		r.dirs = append(r.dirs, dir)
		r.mu.Unlock()
		return prev(dir, acl)
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

// waitForGrants blocks until no grant is running on dir.
func waitForGrants(dir string) {
	slot := grantSlot(dir)
	<-slot
	slot <- struct{}{}
}

// holdGrants makes every propagating write wait until release is called, standing
// in for a walk over a large tree. Install it after the recorder.
func holdGrants(t *testing.T, dir string) (started <-chan struct{}, release func()) {
	t.Helper()
	entered := make(chan struct{}, 16)
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	prev := setTreeDACL
	setTreeDACL = func(dir string, acl *windows.ACL) error {
		entered <- struct{}{}
		<-gate
		return prev(dir, acl)
	}
	t.Cleanup(func() {
		release()
		// A write still running must end before the seam and the tree go away.
		waitForGrants(dir)
		setTreeDACL = prev
	})
	return entered, release
}

func reportEvery(t *testing.T, d time.Duration) {
	t.Helper()
	prev := grantReportEvery
	grantReportEvery = d
	t.Cleanup(func() { grantReportEvery = prev })
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

// TestExtraDirectoryRevokedByAnotherLocalIsGrantedAgain covers two Locals on one
// root, which share an identity: closing one revokes the extra directories.
func TestExtraDirectoryRevokedByAnotherLocalIsGrantedAgain(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	extra := t.TempDir()
	sid := sidFor(t, l)
	ctx := context.Background()
	if err := l.grantDir(ctx, extra, sid); err != nil {
		t.Fatal(err)
	}
	if err := revokeDirAccess(extra, sid); err != nil {
		t.Fatal(err)
	}
	if err := l.grantDir(ctx, extra, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(extra); got != 2 {
		t.Fatalf("a revoked extra directory was written %d times, want 2", got)
	}
}

// TestCancelledGrantReturnsAndTheNextLaunchWaits cancels a grant whose write has
// not returned. The caller must get the cancellation with no marker left, and a
// later launch must wait for the running write rather than start another.
func TestCancelledGrantReturnsAndTheNextLaunchWaits(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	started, release := holdGrants(t, l.root)
	sid := sidFor(t, l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- l.grantDir(ctx, l.root, sid) }()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled grant returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled grant waited for the write")
	}
	if grantMarked(l.root, sid, fileAllAccess) {
		t.Fatal("an unfinished grant left a completion marker")
	}

	next := newGrantLocal(t, l.root)
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if err := next.grantDir(short, l.root, sid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a launch during a running grant returned %v, want it to wait", err)
	}

	release()
	if err := next.grantDir(context.Background(), l.root, sid); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(l.root); got != 1 {
		t.Fatalf("a cancelled grant that finished was written %d times, want 1", got)
	}
}

// TestFailedGrantIsNotTrusted covers a write that fails: the error reaches the
// caller and no marker from an earlier grant survives it.
func TestFailedGrantIsNotTrusted(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	sid := sidFor(t, l)
	ctx := context.Background()
	if err := l.grantDir(ctx, l.root, sid); err != nil {
		t.Fatal(err)
	}
	if err := revokeDirAccess(l.root, sid); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("refused")
	prev := setTreeDACL
	setTreeDACL = func(string, *windows.ACL) error { return refused }
	t.Cleanup(func() { setTreeDACL = prev })
	if err := newGrantLocal(t, l.root).grantDir(ctx, l.root, sid); !errors.Is(err, refused) {
		t.Fatalf("a failed grant returned %v", err)
	}
	if grantMarked(l.root, sid, fileAllAccess) {
		t.Fatal("a failed grant kept the marker of the earlier one")
	}
}

// TestPreCancelledGrantWritesNothing checks a context cancelled before the grant
// starts is honoured.
func TestPreCancelledGrantWritesNothing(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	sid := sidFor(t, l)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.grantDir(ctx, l.root, sid); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant under a cancelled context returned %v, want context.Canceled", err)
	}
	waitForGrants(l.root)
	if got := rec.count(l.root); got != 0 {
		t.Fatalf("a pre-cancelled grant wrote %d times", got)
	}
}

// TestGrantKeepsAProtectedSubdirectoryProtected guards the choice of call that
// setTreeDACL documents: a directory with inheritance turned off must not receive
// the sandbox identity.
func TestGrantKeepsAProtectedSubdirectoryProtected(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	l := newGrantLocal(t, t.TempDir())
	closed := filepath.Join(l.root, "closed")
	if err := os.Mkdir(closed, 0o750); err != nil {
		t.Fatal(err)
	}
	own, err := readDACL(closed)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(closed, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, own, nil); err != nil {
		t.Fatal(err)
	}
	before := sddl(t, closed)

	if err := l.grantDir(context.Background(), l.root, sidFor(t, l)); err != nil {
		t.Fatal(err)
	}
	if after := sddl(t, closed); after != before {
		t.Fatalf("the grant rewrote a protected subdirectory:\n before %s\n after  %s", before, after)
	}
}

func sddl(t *testing.T, dir string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// TestLongGrantIsReported checks the status a host shows instead of a hang, and
// that it is closed off when the grant ends.
func TestLongGrantIsReported(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	reportEvery(t, time.Millisecond)
	l := newGrantLocal(t, t.TempDir())
	_, release := holdGrants(t, l.root)

	var reports []GrantProgress
	ctx := WithGrantProgress(context.Background(), func(p GrantProgress) {
		reports = append(reports, p)
		if len(reports) == 3 {
			release()
		}
	})
	if err := l.grantDir(ctx, l.root, sidFor(t, l)); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 4 {
		t.Fatalf("got %d reports, want several while waiting and an end: %+v", len(reports), reports)
	}
	if first := reports[0]; first.Dir != l.root || first.Elapsed <= 0 || first.Done || !first.First {
		t.Fatalf("first report = %+v", first)
	}
	for _, p := range reports[1:] {
		if p.First {
			t.Fatalf("a later report claims to be the first: %+v", p)
		}
	}
	if last := reports[len(reports)-1]; !last.Done || last.Err != nil || last.Elapsed <= 0 || last.Dir != l.root {
		t.Fatalf("last report = %+v, want a finished grant", last)
	}
}

// TestAbandonedGrantReportsHowItEnded gives a host what it records when the wait
// ends without the grant: the context's error.
func TestAbandonedGrantReportsHowItEnded(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	reportEvery(t, time.Millisecond)
	l := newGrantLocal(t, t.TempDir())
	_, release := holdGrants(t, l.root)
	defer func() {
		release()
		waitForGrants(l.root)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var last GrantProgress
	reporting := WithGrantProgress(ctx, func(p GrantProgress) {
		last = p
		cancel()
	})
	if err := l.grantDir(reporting, l.root, sidFor(t, l)); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant returned %v, want context.Canceled", err)
	}
	if !last.Done || !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("last report = %+v, want Done with the cancellation", last)
	}
}

// TestGrantRootLeavesNothingForTheFirstCommand is what starting the grant when a
// session opens buys: the command that follows writes nothing.
func TestGrantRootLeavesNothingForTheFirstCommand(t *testing.T) {
	for name, opts := range map[string][]LocalOption{
		"appcontainer":     {WithKernelConfinement()},
		"write-restricted": {WithReadOnlyFS(), WithSeccomp(), WithHostReadable()},
	} {
		t.Run(name, func(t *testing.T) {
			var rec grantRecorder
			rec.install(t)
			ctx := context.Background()
			root := t.TempDir()

			ahead := newGrantLocal(t, root, opts...)
			if got := ahead.Containment(); got != ContainmentKernel {
				t.Skipf("containment = %s on this machine, so no command is granted", got)
			}
			if err := ahead.GrantRoot(ctx); err != nil {
				t.Fatal(err)
			}
			if got := rec.count(ahead.root); got != 1 {
				t.Fatalf("GrantRoot wrote the root %d times, want 1", got)
			}
			if err := ahead.Close(); err != nil {
				t.Fatal(err)
			}

			l := newGrantLocal(t, root, opts...)
			if _, err := l.Exec(ctx, Command{Line: "echo hi"}); err != nil {
				t.Fatal(err)
			}
			if got := rec.count(l.root); got != 1 {
				t.Fatalf("the command after GrantRoot wrote the root again: %d writes", got)
			}
		})
	}
}

// TestGrantRootWritesNothingWhenNoCommandWould covers the two Locals whose
// commands never need the grant.
func TestGrantRootWritesNothingWhenNoCommandWould(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	ctx := context.Background()

	plain := newGrantLocal(t, t.TempDir())
	if err := plain.GrantRoot(ctx); err != nil {
		t.Fatalf("an unconfined Local: %v", err)
	}
	// Windows cannot hide a path, so every command of this Local is refused.
	refused := newGrantLocal(t, t.TempDir(), WithDefaultConfinement(), WithUnreadablePaths(t.TempDir()))
	if err := refused.GrantRoot(ctx); !errors.Is(err, ErrReadDeny) {
		t.Fatalf("a Local that refuses commands: GrantRoot returned %v, want ErrReadDeny", err)
	}
	if got := rec.count(refused.root); got != 0 {
		t.Fatalf("a refused Local wrote the root %d times", got)
	}
	if got := rec.count(plain.root); got != 0 {
		t.Fatalf("an unconfined Local wrote the root %d times", got)
	}
}

// TestExtraDirectoriesAreNotRewrittenPerCommand keeps the grant each launch makes
// on its extra directories to a read once they are in place.
func TestExtraDirectoriesAreNotRewrittenPerCommand(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	writable, readable := t.TempDir(), t.TempDir()
	l := newGrantLocal(t, t.TempDir(), WithWritableDir(writable), WithReadableDir(readable))
	ctx := context.Background()
	sid := sidFor(t, l)
	for range 3 {
		if err := l.grantRestrictedWritableDirs(ctx); err != nil {
			t.Fatal(err)
		}
		if err := l.grantReadableDirs(ctx, sid); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := l.writableTargets()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range append(targets, l.readableDirs...) {
		if got := rec.count(dir); got != 1 {
			t.Errorf("%s was written %d times over three launches, want 1", dir, got)
		}
	}
}

// TestQuickGrantIsNotReported keeps a small directory from flashing a status.
func TestQuickGrantIsNotReported(t *testing.T) {
	var rec grantRecorder
	rec.install(t)
	reportEvery(t, time.Hour)
	l := newGrantLocal(t, t.TempDir())
	ctx := WithGrantProgress(context.Background(), func(p GrantProgress) { t.Errorf("reported %+v", p) })
	if err := l.grantDir(ctx, l.root, sidFor(t, l)); err != nil {
		t.Fatal(err)
	}
}

// TestCancelledLaunchDoesNotFallBack keeps a user's cancellation from being read
// as the host refusing confinement.
func TestCancelledLaunchDoesNotFallBack(t *testing.T) {
	l := newGrantLocal(t, t.TempDir(), WithBestEffortConfinement(func(err error) { t.Errorf("fell back: %v", err) }))
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if l.fallBack(fmt.Errorf("sandbox: grant working directory: %w", err), true) {
			t.Fatalf("%v ran the command at the floor", err)
		}
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
