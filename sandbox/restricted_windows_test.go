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

// canonicalizeProbeSource is a program that performs the path canonicalization Windows
// programs do on startup (the call behind Rust's std::fs::canonicalize): open the file,
// then map its handle back to a DOS path. Under an AppContainer that mapping fails with
// access-denied, which is the whole reason the write-restricted tier exists; under the
// write-restricted tier it must succeed. It prints one line, either the canonical path or
// the error, so the test asserts on the primitive itself rather than on a proxy for it.
const canonicalizeProbeSource = `package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func main() {
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Println("OPEN-ERR", err)
		return
	}
	defer f.Close()
	buf := make([]uint16, 1024)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], 1024, 0)
	if err != nil {
		fmt.Println("CANONICALIZE-ERR", err)
		return
	}
	fmt.Println("CANONICALIZED", windows.UTF16ToString(buf[:n]))
}
`

// buildCanonicalizeProbe compiles the probe program into dir and returns its path. It
// builds against this module (so the x/sys/windows dependency resolves from the module
// cache) and skips the test where no Go toolchain is available to build with.
func buildCanonicalizeProbe(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the canonicalize probe")
	}
	src := filepath.Join(t.TempDir(), "canonprobe")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(canonicalizeProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "canonprobe.exe")
	wd, err := os.Getwd() // the sandbox package dir: build inside this module
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", out, filepath.Join(src, "main.go"))
	cmd.Dir = wd
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build canonicalize probe: %v: %s", err, b)
	}
	return out
}

// TestWriteRestrictedCanonicalizesPaths is the regression test for the defect that
// motivates the tier: inside an AppContainer a confined child cannot map a file handle
// back to a DOS path, so every program that canonicalizes a path on startup dies. Under
// WithHostReadable the same call must succeed.
func TestWriteRestrictedCanonicalizesPaths(t *testing.T) {
	root := t.TempDir()
	probe := buildCanonicalizeProbe(t, root)
	target := filepath.Join(root, "config.toml")
	if err := os.WriteFile(target, []byte("# probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loc, err := NewLocal(root, WithReadOnlyFS(), WithSeccomp(), WithHostReadable(), WithExecTimeout(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loc.Close() }()

	res, err := loc.Capture(context.Background(), CaptureSpec{Argv: []string{probe, target}})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(res.Output, "CANONICALIZED") {
		t.Fatalf("canonicalization failed under the write-restricted tier: %s", res.Output)
	}
}

// TestWriteRestrictedConfinesWrites proves the tier still earns its name: the workspace is
// writable, the host outside it is not, and reads of the host succeed (the weakening the
// tier trades for, and the reason it is opt-in).
func TestWriteRestrictedConfinesWrites(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	loc, err := NewLocal(root, WithReadOnlyFS(), WithSeccomp(), WithHostReadable(), WithExecTimeout(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loc.Close() }()
	ctx := context.Background()

	if _, err := loc.Exec(ctx, Command{Line: "echo confined > inside.txt"}); err != nil {
		t.Fatalf("workspace write refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "inside.txt")); err != nil {
		t.Fatalf("workspace write did not land: %v", err)
	}

	outFile := filepath.Join(outside, "escaped.txt")
	res, err := loc.Exec(ctx, Command{Line: "echo escaped > " + outFile})
	if err == nil && res.ExitCode == 0 {
		t.Error("write outside the workspace succeeded; the write gate does not hold")
	}
	if _, err := os.Stat(outFile); err == nil {
		t.Error("a file outside the workspace was created by the confined child")
	}

	res, err = loc.Exec(ctx, Command{Line: `type C:\Windows\win.ini`})
	if err != nil || res.ExitCode != 0 {
		t.Errorf("host read refused under the write-restricted tier: err=%v exit=%d out=%s", err, res.ExitCode, res.Output)
	}
}

// TestWriteRestrictedTierIsReported proves a governed run can tell the two Windows tiers
// apart in its record: they bound an exploit alike but confine reads differently, so the
// name has to distinguish them.
func TestWriteRestrictedTierIsReported(t *testing.T) {
	confined, err := NewLocal(t.TempDir(), WithReadOnlyFS(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = confined.Close() }()
	if got := confined.ConfinementTier(); got != "appcontainer" {
		t.Errorf("default tier = %q, want appcontainer", got)
	}
	if got := confined.Containment(); got != ContainmentKernel {
		t.Errorf("default containment = %v, want kernel", got)
	}

	readable, err := NewLocal(t.TempDir(), WithReadOnlyFS(), WithSeccomp(), WithHostReadable())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readable.Close() }()
	if got := readable.ConfinementTier(); got != "write-restricted-token" {
		t.Errorf("host-readable tier = %q, want write-restricted-token", got)
	}
	if got := readable.Containment(); got != ContainmentKernel {
		t.Errorf("host-readable containment = %v, want kernel", got)
	}
}

// TestWorkspaceRestrictSIDIsUniqueAndStable pins the property the write gate rests on: a
// workspace's restricting identity is stable across launches (or its grant would not
// match) and unique per workspace (or one sandbox's child could write another's tree,
// since a workspace grants write to its own restricting SID).
func TestWorkspaceRestrictSIDIsUniqueAndStable(t *testing.T) {
	a1, err := workspaceRestrictSID(`C:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := workspaceRestrictSID(`C:\work\alpha`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := workspaceRestrictSID(`C:\work\beta`)
	if err != nil {
		t.Fatal(err)
	}
	if a1.String() != a2.String() {
		t.Errorf("restricting sid is not stable for one root: %s vs %s", a1, a2)
	}
	if a1.String() == b.String() {
		t.Errorf("two roots share a restricting sid: %s", a1)
	}
	if !strings.HasPrefix(a1.String(), "S-1-5-80-") {
		t.Errorf("restricting sid %s is not in the service-account namespace; package and capability sids are rejected by CreateRestrictedToken", a1)
	}
}
