//go:build darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestConfinedCommandRuns proves the generated profile is well formed and accepted by
// the sandbox launcher: an ordinary command run under the full kernel-confined preset
// executes and produces its output. A malformed profile would make the launcher exit
// before the command runs, so this is the guard that the profile actually compiles.
func TestConfinedCommandRuns(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: "echo confined"})
	if err != nil {
		t.Fatalf("a benign confined command must run: %v", err)
	}
	if res.ExitCode != 0 || res.Output != "confined\n" {
		t.Fatalf("an ordinary command must run unaffected under confinement, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestConfinedReportsKernelContainment confirms the macOS adapter raises the reported
// containment to kernel-confined under the full preset, so the run gate treats it as a
// T1 tier rather than a bare process jail.
func TestConfinedReportsKernelContainment(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	if got := sb.Containment(); got != ContainmentKernel {
		t.Fatalf("a fully confined macOS sandbox must report kernel-confined, got %s", got)
	}
}

// TestReadOnlyFSWritesOnlyWorkdir proves the filesystem confinement: a command run
// under WithReadOnlyFS can write its own working directory but cannot write anywhere
// else on the host, and the host stays readable, so the confinement restricts writes
// without blinding the command.
func TestReadOnlyFSWritesOnlyWorkdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir() // a sibling the test user owns, outside the sandbox root

	sb, err := NewLocal(root, WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: "echo inside > made.txt"})
	if err != nil {
		t.Fatalf("workdir write exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a write to the working directory must succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(sb.Root(), "made.txt")); err != nil {
		t.Fatalf("the working-directory write did not land: %v", err)
	}

	// A write outside the working tree is refused: the host is read-only.
	res, err = sb.Exec(ctx, Command{Line: "echo escape > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail under a read-only host, but it succeeded:\n%s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); err == nil {
		t.Fatal("a file was written outside the working tree under a read-only host")
	}

	// Reads still work: confinement restricts writes, it does not blind the command.
	res, err = sb.Exec(ctx, Command{Line: "cat /etc/hosts > /dev/null"})
	if err != nil {
		t.Fatalf("read exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a read of a host file must still succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestNetworkDeniedBlocksConnect confirms a command cannot open an outbound connection
// when the network is denied. A non-zero exit is the pass.
func TestNetworkDeniedBlocksConnect(t *testing.T) {
	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	res, err := denied.Exec(context.Background(), Command{Line: "curl --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}

	// Sanity: without the option the same command can reach the network, so the test is
	// observing the denial and not a runner that simply has no network.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(context.Background(), Command{Line: "curl --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("runner has no outbound network without the option, cannot distinguish here:\n%s", res.Output)
	}
}

// TestNetworkDeniedComposesWithReadOnly confirms the two confinements apply together:
// a command run under both cannot reach the network or write outside its working tree.
func TestNetworkDeniedComposesWithReadOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()

	sb, err := NewLocal(root, WithReadOnlyFS(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: "curl --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"})
	if err != nil {
		t.Fatalf("connect exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny:\n%s", res.Output)
	}

	res, err = sb.Exec(ctx, Command{Line: "echo escape > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail when the host is read-only:\n%s", res.Output)
	}
}
