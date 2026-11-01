//go:build windows

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// confinementTiers are the two ways a Windows command is confined. The default baseline
// uses the write-restricted token; the AppContainer is the stricter tier a kernel-confined
// caller selects. Both build the child's environment through the same function, but each
// has its own grant path, so each is exercised.
var confinementTiers = map[string][]LocalOption{
	"write-restricted token": {WithDefaultConfinement()},
	"appcontainer":           {WithReadOnlyFS(), WithSeccomp()},
}

// execLine runs a command line in l and returns its trimmed output, failing the test on a
// launch error.
func execLine(t *testing.T, l *Local, line string) string {
	t.Helper()
	res, err := l.Exec(context.Background(), Command{Line: line})
	if err != nil {
		t.Fatalf("Exec %q: %v", line, err)
	}
	return strings.TrimSpace(res.Output)
}

// within reports whether path is dir or lies under it, comparing case-insensitively as
// Windows paths are.
func within(path, dir string) bool {
	p, d := strings.ToLower(filepath.Clean(path)), strings.ToLower(filepath.Clean(dir))
	return p == d || strings.HasPrefix(p, d+string(filepath.Separator))
}

// TestConfinedTempIsNotTheWorkspace guards the defect behind "ignoring go.mod in system
// temp root": a child whose TEMP is the workspace root cannot use a Go module there, since
// Go refuses a go.mod that sits at the temp root. The temp directory has to be a real,
// writable place that is not the workspace.
func TestConfinedTempIsNotTheWorkspace(t *testing.T) {
	for name, opts := range confinementTiers {
		t.Run(name, func(t *testing.T) {
			l := newTestLocal(t, opts...)
			for _, v := range []string{"TEMP", "TMP"} {
				got := execLine(t, l, "echo %"+v+"%")
				if got == "" || got == "%"+v+"%" {
					t.Fatalf("%s is not set in the confined child: %q", v, got)
				}
				if within(got, l.Root()) {
					t.Fatalf("%s=%s is inside the workspace %s; it must be a directory outside it", v, got, l.Root())
				}
			}
			tmp := execLine(t, l, "echo %TEMP%")
			probe := `echo scratch>"` + tmp + `\probe.txt" && type "` + tmp + `\probe.txt"`
			if out := execLine(t, l, probe); !strings.Contains(out, "scratch") {
				t.Fatalf("the child cannot write its TEMP %s: %q", tmp, out)
			}
		})
	}
}

// TestToolchainCachesPointIntoOneScratchDir proves the cache variables of the toolchains
// that default under the user profile resolve to the sandbox's scratch directory, the one
// writable place outside the workspace, rather than to a profile path the child cannot
// write.
func TestToolchainCachesPointIntoOneScratchDir(t *testing.T) {
	for name, opts := range confinementTiers {
		t.Run(name, func(t *testing.T) {
			l := newTestLocal(t, opts...)
			scratch, err := l.scratchDir()
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []string{"GOCACHE", "GOMODCACHE", "npm_config_cache", "PIP_CACHE_DIR"} {
				got := execLine(t, l, "echo %"+v+"%")
				if got == "" || got == "%"+v+"%" {
					t.Fatalf("%s is not set in the confined child: %q", v, got)
				}
				if !within(got, scratch) {
					t.Fatalf("%s=%s is outside the scratch directory %s", v, got, scratch)
				}
			}
		})
	}
}

// tinyModule writes a module with one passing test into dir.
func tinyModule(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"go.mod":       "module example.com/tiny\n\ngo 1.21\n",
		"tiny.go":      "package tiny\n\nfunc Add(a, b int) int { return a + b }\n",
		"tiny_test.go": "package tiny\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad sum\")\n\t}\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestGoTestRunsInDefaultConfinement is the end-to-end case from the field report: under
// the default baseline, `go env GOCACHE` names a directory the child can write and
// `go test` on a module in the workspace compiles and passes. Before the scratch
// directory, the build cache could not be created and the workspace was the temp root.
func TestGoTestRunsInDefaultConfinement(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	l := newTestLocal(t, WithDefaultConfinement(), WithExecTimeout(3*time.Minute))
	tinyModule(t, l.Root())

	tmp := execLine(t, l, "echo %TEMP%")
	if cache := execLine(t, l, "go env GOCACHE"); !within(cache, filepath.Dir(tmp)) {
		t.Fatalf("go env GOCACHE = %q, want a directory under the scratch dir %s", cache, filepath.Dir(tmp))
	}
	res, err := l.Exec(context.Background(), Command{Line: "go test ./..."})
	if err != nil {
		t.Fatalf("Exec go test: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("go test failed in the default confinement (exit %d):\n%s", res.ExitCode, res.Output)
	}
}

// TestScratchDirIsPerSandboxAndRemovedOnClose guards the lifetime the threat model states:
// each sandbox has its own scratch directory, and nothing in it survives Close, including
// read-only files the way the Go module cache writes them.
func TestScratchDirIsPerSandboxAndRemovedOnClose(t *testing.T) {
	a := newTestLocal(t, WithDefaultConfinement())
	b := newTestLocal(t, WithDefaultConfinement())
	da, err := a.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatalf("two sandboxes share the scratch directory %s", da)
	}
	if within(da, a.Root()) {
		t.Fatalf("scratch directory %s is inside the workspace %s", da, a.Root())
	}

	ro := filepath.Join(da, "go-mod", "cache", "x.txt")
	if err := os.MkdirAll(filepath.Dir(ro), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ro, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(da); !os.IsNotExist(err) {
		t.Fatalf("scratch directory %s survived Close (stat err %v)", da, err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("closing one sandbox removed another's scratch directory: %v", err)
	}
}

// TestScratchEnvKeepsOperatorValues proves a value the operator granted is not replaced by
// the scratch default, including when the name differs only in case, while the host's own
// TEMP, which the baseline copies in, is replaced.
func TestScratchEnvKeepsOperatorValues(t *testing.T) {
	host := map[string]string{"TEMP": `C:\host\tmp`, "TMP": `C:\host\tmp`}
	m := map[string]string{
		"TEMP":    `C:\host\tmp`,
		"TMP":     `D:\operator\tmp`,
		"gocache": `D:\operator\cache`,
	}
	withScratchEnv(m, `S:\scratch`, func(k string) string { return host[k] })

	if m["TEMP"] != `S:\scratch\tmp` {
		t.Errorf("host TEMP kept: %q", m["TEMP"])
	}
	if m["TMP"] != `D:\operator\tmp` {
		t.Errorf("operator TMP overridden: %q", m["TMP"])
	}
	if _, dup := m["GOCACHE"]; dup || m["gocache"] != `D:\operator\cache` {
		t.Errorf("operator gocache overridden or duplicated: %v", m)
	}
	if m["GOMODCACHE"] != `S:\scratch\go-mod` {
		t.Errorf("GOMODCACHE = %q, want the scratch default", m["GOMODCACHE"])
	}
}

// TestScratchDirIsNotWritableFromAnotherSandbox proves the grant is per sandbox: a child
// of one sandbox cannot write another sandbox's scratch directory.
func TestScratchDirIsNotWritableFromAnotherSandbox(t *testing.T) {
	for name, opts := range confinementTiers {
		t.Run(name, func(t *testing.T) {
			owner := newTestLocal(t, opts...)
			other := newTestLocal(t, opts...)
			dir, err := owner.scratchDir()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.Exec(context.Background(), Command{Line: `echo x>"` + filepath.Join(dir, "intruder.txt") + `"`}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "intruder.txt")); err == nil {
				t.Fatalf("a child of another sandbox wrote into %s", dir)
			}
		})
	}
}
