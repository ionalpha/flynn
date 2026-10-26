//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// requireUnprivileged skips a test that calls a mount routine in the test process itself.
// An unprivileged process is refused every mount, which is what these tests rely on; a
// process that may mount would change the mount table of the machine running the suite.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: the mount routines would succeed against the real mount table")
	}
}

// TestLauncherRefusesMalformedUnreadablePaths shows a launcher handed an undecodable
// unreadable-path list stops with the launcher's failure status before any mount: a set it
// could not read must not become an empty set, which would run the command with nothing hidden.
func TestLauncherRefusesMalformedUnreadablePaths(t *testing.T) {
	t.Setenv(envDir, t.TempDir())
	t.Setenv(envArgv, encodeArgv([]string{"true"}))
	t.Setenv(envReadonly, "1")
	t.Setenv(envWritable, "")
	t.Setenv(envHidden, "!!not base64!!")
	if code := runConfinedChild(); code != 126 {
		t.Fatalf("runConfinedChild = %d, want 126", code)
	}
}

// TestConfineMountsFailsWhenTheMountTreeCannotBeMadePrivate shows the first step's failure
// is reported, so a host that refuses it is named.
func TestConfineMountsFailsWhenTheMountTreeCannotBeMadePrivate(t *testing.T) {
	requireUnprivileged(t)
	err := confineMounts(t.TempDir(), nil, nil)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("confineMounts = %v, want EPERM for an unprivileged caller", err)
	}
}

// TestKeptMountFlagsFallsBackToNoneForAnUnreadablePath holds the documented fallback: a
// path statfs cannot read keeps no flag, since keeping a flag only ever takes a right away.
func TestKeptMountFlagsFallsBackToNoneForAnUnreadablePath(t *testing.T) {
	if got := keptMountFlags(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Fatalf("keptMountFlags(missing) = %#x, want 0", got)
	}
	if got := keptMountFlags(t.TempDir()); got&^restrictiveMountFlags != 0 {
		t.Fatalf("keptMountFlags returned %#x, which holds a flag that is not restrictive", got)
	}
}

// TestHideUnreadableFailsLoudly holds the rule that a path which was to be hidden and could
// not be fails the launch, for each kind of path the launcher covers.
func TestHideUnreadableFailsLoudly(t *testing.T) {
	requireUnprivileged(t)
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"a directory":               dir,
		"a file":                    file,
		"a path under a plain file": filepath.Join(file, "below"),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := hideUnreadable([]string{p}); err == nil {
				t.Fatalf("hiding %s succeeded without privilege", p)
			}
		})
	}
	if err := hideUnreadable([]string{filepath.Join(base, "missing")}); err != nil {
		t.Fatalf("a path that does not exist has nothing to hide, got %v", err)
	}
}
