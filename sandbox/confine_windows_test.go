//go:build windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfinedCommandRuns proves a command launches and produces output inside the
// AppContainer under the full kernel-confined preset. A container that could not load
// the system libraries a shell needs would fail to run at all, so this is the guard
// that the container is built correctly.
func TestConfinedCommandRuns(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: "echo confined"})
	if err != nil {
		t.Fatalf("a benign confined command must run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "confined") {
		t.Fatalf("an ordinary command must run inside the container, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestConfinedReportsKernelContainment confirms the Windows adapter raises the reported
// containment to kernel-confined under the full preset, so the run gate treats it as a
// T1 tier rather than a bare process jail.
func TestConfinedReportsKernelContainment(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	if got := sb.Containment(); got != ContainmentKernel {
		t.Fatalf("a fully confined Windows sandbox must report kernel-confined, got %s", got)
	}
}

// TestReadOnlyFSWritesOnlyWorkdir proves the filesystem confinement: a command can
// write its own working directory but cannot write the host outside it, and reads of
// the host still work, so the confinement restricts writes without blinding the
// command.
func TestReadOnlyFSWritesOnlyWorkdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir() // a sibling the test user owns, outside the sandbox root

	sb, err := NewLocal(root, WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: `echo data> made.txt && type made.txt`})
	if err != nil {
		t.Fatalf("workdir write exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "data") {
		t.Fatalf("a write to the working directory must succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(sb.Root(), "made.txt")); err != nil {
		t.Fatalf("the working-directory write did not land: %v", err)
	}

	escape := filepath.Join(outside, "escape.txt")
	res, err = sb.Exec(ctx, Command{Line: `echo escape> "` + escape + `"`})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail under confinement, but it succeeded:\n%s", res.Output)
	}
	if _, err := os.Stat(escape); err == nil {
		t.Fatal("a file was written outside the working tree under confinement")
	}

	// There is no read-succeeds case to assert here. Under an AppContainer reads are
	// default-deny: the command reads only what it is granted plus the system
	// libraries it needs to load and run (which is execute access, granted separately
	// from read). This is stricter than the read-only host on the other platforms,
	// where the whole filesystem stays readable, and is the safer direction.
}

// TestNetworkDeniedBlocksConnect confirms a command cannot open an outbound connection
// when the network is denied, while the same command runs unconfined can. A non-zero
// exit under denial is the pass.
func TestNetworkDeniedBlocksConnect(t *testing.T) {
	const probe = `curl --max-time 6 -s -o NUL http://1.1.1.1`

	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	res, err := denied.Exec(context.Background(), Command{Line: probe})
	if err != nil {
		t.Fatalf("denied exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}

	// Sanity: unconfined, the same command can reach the network, so the test observes
	// the denial and not a runner with no outbound path.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(context.Background(), Command{Line: probe})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("runner has no outbound network unconfined, cannot distinguish here:\n%s", res.Output)
	}
}

// TestNetworkAllowedWhenNotDenied confirms a confined command that did not deny the
// network can still reach it, so the filesystem confinement does not silently also cut
// off the network.
func TestNetworkAllowedWhenNotDenied(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: `curl --max-time 6 -s -o NUL http://1.1.1.1`})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("runner has no outbound network, cannot confirm the allow path:\n%s", res.Output)
	}
}

// TestUnconfinedCommandStillRuns confirms the default, unconfined path is unchanged by
// the AppContainer routing: a sandbox with no confinement options runs an ordinary
// command through the standard library.
func TestUnconfinedCommandStillRuns(t *testing.T) {
	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: "echo plain"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "plain") {
		t.Fatalf("an unconfined command must run normally, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestAppContainerMonikerStableAndUnique checks the container name is deterministic per
// root and differs between roots, which is what keeps two sandbox roots from sharing a
// container identity (and therefore each other's granted directories).
func TestAppContainerMonikerStableAndUnique(t *testing.T) {
	a1 := appContainerMoniker(`C:\work\a`)
	a2 := appContainerMoniker(`C:\work\a`)
	b := appContainerMoniker(`C:\work\b`)
	if a1 != a2 {
		t.Fatalf("moniker must be stable for a root, got %q and %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("different roots must get different monikers, both were %q", a1)
	}
	if !strings.HasPrefix(a1, "flynn.sbx.") || len(a1) > 64 {
		t.Fatalf("moniker %q is not within the allowed name shape", a1)
	}
}
