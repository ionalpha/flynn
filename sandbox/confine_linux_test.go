//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNetworkDeniedHasNoRoutes proves the kernel enforcement: a command run under
// WithNetworkDenied lands in a network namespace with no routes, so it cannot reach
// anything, while the same command without the option sees the host's routes. The
// test skips where unprivileged user namespaces are unavailable (some locked-down
// hosts), so it verifies the isolation where the kernel allows it without failing
// where it cannot be set up at all.
func TestNetworkDeniedHasNoRoutes(t *testing.T) {
	ctx := context.Background()

	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	res, err := denied.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/network namespaces unavailable on this host")
		}
		t.Fatalf("denied exec: %v", err)
	}
	if n := routeEntries(res.Output); n != 0 {
		t.Fatalf("a network-denied command must see no routes, saw %d:\n%s", n, res.Output)
	}

	// Sanity: a normal command on the same host does have routes, so the test is
	// actually observing the isolation and not a host that simply has none.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if routeEntries(res.Output) == 0 {
		t.Skip("host itself has no routes; cannot distinguish isolation here")
	}
}

// TestNetworkDeniedBlocksConnect confirms a command cannot open an outbound
// connection when the network is denied.
func TestNetworkDeniedBlocksConnect(t *testing.T) {
	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	// A non-loopback connect must fail with no route; a non-zero exit is the pass.
	res, err := denied.Exec(context.Background(), Command{Line: "timeout 3 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>&1"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/network namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}
}

// TestReadOnlyFSWritesOnlyWorkdir proves the filesystem confinement: a command run
// under WithReadOnlyFS can write its own working directory but cannot write anywhere
// else on the host, even to a directory the running user owns (which a plain user
// namespace would leave writable). It also confirms the host stays readable, so the
// confinement restricts writes without blinding the command.
func TestReadOnlyFSWritesOnlyWorkdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir() // a sibling the test user owns, outside the sandbox root

	sb, err := NewLocal(root, WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}

	// A write inside the working directory succeeds and lands on disk.
	res, err := sb.Exec(ctx, Command{Line: "echo inside > made.txt"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/mount namespaces unavailable on this host")
		}
		t.Fatalf("workdir write exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a write to the working directory must succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "made.txt")); err != nil {
		t.Fatalf("the working-directory write did not land: %v", err)
	}

	// A write to a user-owned directory outside the working tree is refused: the
	// host is read-only, so this is the gap a plain user namespace would leave open.
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
	res, err = sb.Exec(ctx, Command{Line: "cat /proc/self/status > /dev/null"})
	if err != nil {
		t.Fatalf("read exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a read of a host file must still succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestReadOnlyFSWithNetworkDenied confirms the two kernel confinements compose: a
// command run under both sees no routes and cannot write outside its working tree.
func TestReadOnlyFSWithNetworkDenied(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()

	sb, err := NewLocal(root, WithReadOnlyFS(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/mount/network namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if n := routeEntries(res.Output); n != 0 {
		t.Fatalf("a network-denied command must see no routes, saw %d:\n%s", n, res.Output)
	}

	res, err = sb.Exec(ctx, Command{Line: "echo escape > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail when the host is read-only:\n%s", res.Output)
	}
}

// TestSeccompBlocksDangerousSyscall proves the syscall filter: a command run under
// WithSeccomp cannot make a denied syscall (here unshare, which creates new
// namespaces), while the same command without the filter can. A refused call fails
// rather than killing the command, so a non-zero exit is the pass. The test is a
// differential so it observes the filter and not a host that forbids the call anyway.
func TestSeccompBlocksDangerousSyscall(t *testing.T) {
	ctx := context.Background()

	filtered, err := NewLocal(t.TempDir(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	res, err := filtered.Exec(ctx, Command{Line: "unshare --user --map-root-user true"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("filtered exec: %v", err)
	}
	if strings.Contains(res.Output, "not found") {
		t.Skip("unshare command not available on this host")
	}
	if res.ExitCode == 0 {
		t.Fatalf("a denied syscall must fail under the filter, but unshare succeeded:\n%s", res.Output)
	}

	// Sanity: without the filter the same command succeeds, so the test is observing
	// the filter and not a host that simply forbids unshare.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(ctx, Command{Line: "unshare --user --map-root-user true"})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("host forbids unshare without the filter, cannot distinguish here:\n%s", res.Output)
	}
}

// TestSeccompAllowsOrdinaryCommand confirms the filter does not break normal work: a
// command using only ordinary syscalls runs and produces its output unaffected.
func TestSeccompAllowsOrdinaryCommand(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: "echo confined && cat /proc/self/status > /dev/null"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "confined") {
		t.Fatalf("an ordinary command must run unaffected under the filter, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// routeEntries counts route table rows in /proc/net/route, excluding the header, so
// zero means the namespace has no routes at all.
func routeEntries(procNetRoute string) int {
	lines := strings.Split(strings.TrimRight(procNetRoute, "\n"), "\n")
	if len(lines) <= 1 {
		return 0
	}
	return len(lines) - 1
}

// namespaceUnavailable reports whether an exec error is a namespace-setup failure
// (the host forbids unprivileged user namespaces) rather than a real problem.
func namespaceUnavailable(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "invalid argument") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "clone")
}
