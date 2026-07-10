//go:build windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// registerProfile registers the AppContainer profile for a workspace path and returns its
// moniker, unregistering it when the test ends.
func registerProfile(t *testing.T, root string) string {
	t.Helper()
	moniker := appContainerMoniker(root)
	sid, err := createOrDeriveACSID(moniker)
	if err != nil {
		t.Fatalf("register profile: %v", err)
	}
	_ = windows.FreeSid(sid)
	t.Cleanup(func() { _ = deleteAppContainerProfile(moniker) })
	return moniker
}

// TestConfinedRunLeavesNoProfile is the regression guard for the leak: a confined command
// that runs and is Closed must leave the machine with exactly as many registered profiles
// as it found. Before the fix, every confined command added one, and a dev box had
// thousands.
func TestConfinedRunLeavesNoProfile(t *testing.T) {
	before := LiveProfileCount()

	l, err := NewLocal(t.TempDir(), WithDefaultConfinement())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	bin := copyHelperInto(t, l.Root())
	if _, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  []string{"SANDBOX_STREAM_HELPER=1"},
	}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close reported a teardown failure: %v", err)
	}

	if after := LiveProfileCount(); after != before {
		t.Fatalf("a confined run leaked %d AppContainer profile(s) (before=%d after=%d)", after-before, before, after)
	}
}

// TestCloseReportsAProfileThatCannotBeDeleted proves the delete failure reaches the caller
// rather than being dropped. A moniker Windows rejects outright stands in for a profile
// that will not go away; before the fix the HRESULT was discarded and Close returned nil.
func TestCloseReportsAProfileThatCannotBeDeleted(t *testing.T) {
	if err := deleteAppContainerProfile(`moniker\with/path:chars`); err == nil {
		t.Fatalf("deleting an invalid profile must report the failure, not swallow it")
	}
	// Deleting one that simply does not exist is a success, so a normal Close over a
	// sandbox that never registered a profile stays quiet.
	if err := deleteAppContainerProfile(profilePrefix + "0000000000000000"); err != nil {
		t.Fatalf("deleting an absent profile must succeed, got %v", err)
	}
}

// TestCleanStaleProfilesCollectsOnlyOldOnes proves the janitor collects a profile left
// behind by a run that never Closed, and leaves a fresh one alone: a live sandbox's
// profile must survive a sweep running concurrently with it.
func TestCleanStaleProfilesCollectsOnlyOldOnes(t *testing.T) {
	root, err := profileRoot()
	if err != nil {
		t.Skipf("no profile root: %v", err)
	}

	// A profile registered and never Closed: exactly the leak. And a second one standing
	// in for a live sandbox's, registered at the same moment.
	stale := registerProfile(t, t.TempDir())
	fresh := registerProfile(t, t.TempDir())

	// Age the stale one past the cutoff by backdating its folder, then sweep with a cutoff
	// between the two.
	staleDir := filepath.Join(root, stale)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Skipf("cannot backdate the profile folder: %v", err)
	}
	cutoff := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

	removed, err := CleanStaleProfiles(cutoff)
	if err != nil {
		t.Fatalf("CleanStaleProfiles: %v", err)
	}
	if removed < 1 {
		t.Fatalf("the janitor collected nothing; the leaked profile is still registered")
	}
	if _, err := os.Stat(staleDir); err == nil {
		t.Fatalf("the stale profile %s survived the sweep", stale)
	}
	if _, err := os.Stat(filepath.Join(root, fresh)); err != nil {
		t.Fatalf("the janitor collected a profile newer than the cutoff, which could be a live sandbox: %v", err)
	}
}

// TestIsProfileMonikerRefusesForeignNames is the guard on the janitor's blast radius: it
// deletes directories under the user's Packages folder, where every other AppContainer
// application on the machine also keeps its state.
func TestIsProfileMonikerRefusesForeignNames(t *testing.T) {
	good := []string{profilePrefix + "0123456789abcdef"}
	bad := []string{
		"",
		"flynn.sbx.",                        // no digest
		profilePrefix + "0123456789abcde",   // too short
		profilePrefix + "0123456789abcdef0", // too long
		profilePrefix + "0123456789ABCDEF",  // digest is lowercase hex
		profilePrefix + "0123456789abcdeg",  // not hex
		"flynn.sbx0123456789abcdef",         // prefix not matched
		"Microsoft.WindowsCalculator_8wekyb3d8bbwe",
		"flynn.sbx.0123456789abcdef.extra",
	}
	for _, n := range good {
		if !isProfileMoniker(n) {
			t.Fatalf("%q must be recognized as this package's profile", n)
		}
	}
	for _, n := range bad {
		if isProfileMoniker(n) {
			t.Fatalf("%q must NOT be treated as this package's profile; the janitor would delete it", n)
		}
	}
}
