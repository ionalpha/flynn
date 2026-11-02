//go:build linux || darwin

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scratchVars are the variables a confined child must find pointing into the scratch
// directory: the temp directory and every toolchain cache.
var scratchVars = []string{"TMPDIR", "GOCACHE", "GOMODCACHE", "npm_config_cache", "PIP_CACHE_DIR"}

// confinedScratchLocal returns a Local under the default kernel confinement, skipping on a
// Linux host that refuses user namespaces. macOS has no such host to skip for, so a refusal
// there fails the test.
func confinedScratchLocal(t *testing.T, opts ...LocalOption) *Local {
	t.Helper()
	l := newTestLocal(t, append([]LocalOption{WithReadOnlyFS(), WithSeccomp()}, opts...)...)
	if !l.kernelConfinementEnforceable() {
		if runtime.GOOS == "linux" {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatal("the kernel confinement is not enforceable on this host")
	}
	return l
}

// runLine executes a command line in l and returns its trimmed output, failing the test on
// a launch error.
func (l *Local) runLine(t *testing.T, line string) string {
	t.Helper()
	res, err := l.Exec(context.Background(), Command{Line: line})
	if err != nil {
		t.Fatalf("Exec %q: %v", line, err)
	}
	return strings.TrimSpace(res.Output)
}

// TestConfinedChildWritesTempAndCaches is the field failure: under the default read-only host
// the child's TMPDIR is the host's and its toolchain caches default under the home
// directory, so `go test`, `npm install` and `pip install` fail on a read-only file system.
// The host temp directory here is a sibling the child cannot write, which is the case on
// macOS (a per-user directory under /var/folders) and on a Linux host whose TMPDIR is not
// /tmp.
func TestConfinedChildWritesTempAndCaches(t *testing.T) {
	hostTemp := t.TempDir()
	t.Setenv("TMPDIR", hostTemp)
	l := confinedScratchLocal(t)

	if out := l.runLine(t, "touch "+filepath.Join(hostTemp, "x")+" 2>&1; echo exit=$?"); strings.Contains(out, "exit=0") {
		t.Fatalf("the host temp directory %s is writable, so this test proves nothing:\n%s", hostTemp, out)
	}

	scratch, err := l.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range scratchVars {
		dir := l.runLine(t, "printf %s \"$"+v+"\"")
		if dir == "" || !pathWithin(dir, scratch) {
			t.Fatalf("%s=%q is not under the scratch directory %s", v, dir, scratch)
		}
		probe := `d="$` + v + `/probe"; mkdir -p "$d" && : > "$d/f" && echo wrote`
		if out := l.runLine(t, probe); out != "wrote" {
			t.Fatalf("the child cannot write %s (%s): %q", v, dir, out)
		}
	}
}

// TestScratchOutlivesOneCommandAndGoesOnClose guards the reason the scratch is a host
// directory: a download in one launch has to be in the cache of the next, and nothing may
// outlive the sandbox, including read-only trees the way the Go module cache writes them.
func TestScratchOutlivesOneCommandAndGoesOnClose(t *testing.T) {
	l := confinedScratchLocal(t)

	l.runLine(t, `mkdir -p "$GOMODCACHE/m/sub" && echo kept > "$GOMODCACHE/m/sub/f" && chmod 444 "$GOMODCACHE/m/sub/f" && chmod 555 "$GOMODCACHE/m/sub" "$GOMODCACHE/m"`)
	if out := l.runLine(t, `cat "$GOMODCACHE/m/sub/f"`); out != "kept" {
		t.Fatalf("a second command did not see the first one's cache: %q", out)
	}

	scratch, err := l.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch directory %s survived Close (stat err %v)", scratch, err)
	}
}

// TestScratchIsPrivateToOneSandbox proves the grant is per sandbox: a child of one sandbox
// finds another sandbox's scratch directory read-only like the rest of the host.
func TestScratchIsPrivateToOneSandbox(t *testing.T) {
	owner := confinedScratchLocal(t)
	other := confinedScratchLocal(t)
	dir, err := owner.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	if out := other.runLine(t, "touch "+filepath.Join(dir, "intruder")+" 2>&1; echo exit=$?"); strings.Contains(out, "exit=0") {
		t.Fatalf("a child of another sandbox wrote into %s:\n%s", dir, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "intruder")); err == nil {
		t.Fatalf("a child of another sandbox created a file in %s", dir)
	}
	if pathWithin(dir, owner.Root()) {
		t.Fatalf("scratch directory %s is inside the workspace %s", dir, owner.Root())
	}
}

// TestScratchKeepsOperatorValues proves a variable the operator granted is not replaced by
// the scratch default, through a real launch.
func TestScratchKeepsOperatorValues(t *testing.T) {
	granted := filepath.Join(t.TempDir(), "mine")
	l := confinedScratchLocal(t, WithEnv(map[string]string{"GOCACHE": granted}), WithWritableDir(filepath.Dir(granted)))
	if got := l.runLine(t, `printf %s "$GOCACHE"`); got != granted {
		t.Fatalf("GOCACHE = %q, want the operator's %q", got, granted)
	}
	if got := l.runLine(t, `printf %s "$GOMODCACHE"`); got == "" {
		t.Fatal("GOMODCACHE was not defaulted beside a granted GOCACHE")
	}
}

// TestNoScratchWithoutReadOnlyHost proves the environment is left alone when the host is not
// read-only: the child can already write its caches where they default, and redirecting them
// would change what an unconfined-filesystem run does for no gain.
func TestNoScratchWithoutReadOnlyHost(t *testing.T) {
	l := newTestLocal(t, WithNetworkDenied())
	res, err := l.Exec(context.Background(), Command{Line: `printf %s "$GOCACHE"`})
	if err != nil {
		if runtime.GOOS == "linux" {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("Exec: %v", err)
	}
	if res.Output != "" {
		t.Fatalf("GOCACHE = %q in a Local that does not confine the filesystem", res.Output)
	}
	if l.scratch.dir != "" {
		t.Fatalf("scratch directory %s created for a Local that does not confine the filesystem", l.scratch.dir)
	}
}

// TestScratchEnvReplacesHostTemp covers the rule on its own: the host's TMPDIR, which the
// baseline copies in, is replaced, an absent one is set, and a different value was granted
// and stays. Names are case-sensitive here, unlike Windows.
func TestScratchEnvReplacesHostTemp(t *testing.T) {
	host := func(string) string { return "/host/tmp" }
	for name, tc := range map[string]struct {
		in   map[string]string
		want string
	}{
		"host value replaced": {map[string]string{"TMPDIR": "/host/tmp"}, "/s/tmp"},
		"absent set":          {map[string]string{}, "/s/tmp"},
		"granted kept":        {map[string]string{"TMPDIR": "/operator/tmp"}, "/operator/tmp"},
	} {
		t.Run(name, func(t *testing.T) {
			withScratchEnv(tc.in, "/s", host)
			if tc.in["TMPDIR"] != tc.want {
				t.Fatalf("TMPDIR = %q, want %q", tc.in["TMPDIR"], tc.want)
			}
			if tc.in["GOCACHE"] != "/s/go-build" {
				t.Fatalf("GOCACHE = %q", tc.in["GOCACHE"])
			}
		})
	}
	m := map[string]string{"gocache": "/operator/cache"}
	withScratchEnv(m, "/s", host)
	if m["GOCACHE"] != "/s/go-build" {
		t.Fatalf("a lowercase gocache must not satisfy GOCACHE on a case-sensitive platform: %v", m)
	}
}

// failScratchCreation makes every scratch directory creation fail for the test.
func failScratchCreation(t *testing.T) {
	t.Helper()
	orig := mkdirTemp
	mkdirTemp = func(string, string) (string, error) { return "", errors.New("no temp root") }
	t.Cleanup(func() { mkdirTemp = orig })
}

// TestScratchFailureRefusesTheLaunch proves a sandbox that cannot create its scratch
// directory refuses to run the command, rather than launching a child that has the
// read-only host and no cache location.
func TestScratchFailureRefusesTheLaunch(t *testing.T) {
	failScratchCreation(t)
	l := newTestLocal(t, WithReadOnlyFS())
	_, err := l.Exec(context.Background(), Command{Line: "true"})
	if err == nil || !strings.Contains(err.Error(), "no temp root") {
		t.Fatalf("Exec error = %v, want the scratch creation failure", err)
	}
}

// TestScratchFallsBackWhenVarTmpIsUnavailable covers the Linux preference for /var/tmp: a
// host without it still gets a scratch directory under the default temp root.
func TestScratchFallsBackWhenVarTmpIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux prefers /var/tmp")
	}
	orig := mkdirTemp
	t.Cleanup(func() { mkdirTemp = orig })
	var parents []string
	mkdirTemp = func(parent, pattern string) (string, error) {
		parents = append(parents, parent)
		if parent == "/var/tmp" {
			return "", errors.New("no /var/tmp")
		}
		return orig(parent, pattern)
	}
	l := newTestLocal(t, WithReadOnlyFS())
	dir, err := l.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 2 || parents[0] != "/var/tmp" || parents[1] != "" {
		t.Fatalf("temp roots tried = %q, want /var/tmp then the default", parents)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

// TestScratchTempSubdirectoryFailureLeavesNothing proves a scratch directory whose temp
// subdirectory cannot be made is removed again and reported, not kept half built.
func TestScratchTempSubdirectoryFailureLeavesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	orig := mkdirTemp
	t.Cleanup(func() { mkdirTemp = orig })
	var made string
	mkdirTemp = func(parent, pattern string) (string, error) {
		dir, err := orig(parent, pattern)
		if err == nil {
			made = dir
			err = os.Chmod(dir, 0o500)
		}
		return dir, err
	}
	l := newTestLocal(t, WithReadOnlyFS())
	if _, err := l.scratchDir(); err == nil {
		t.Fatal("scratchDir succeeded in a directory that cannot hold the temp subdirectory")
	}
	if l.scratch.dir != "" {
		t.Fatalf("scratch directory %s recorded after a failed creation", l.scratch.dir)
	}
	if _, err := os.Stat(made); !os.IsNotExist(err) {
		t.Fatalf("half-built scratch directory %s survived (stat err %v)", made, err)
	}
}
