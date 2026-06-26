//go:build linux

package sandbox

import (
	"context"
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
