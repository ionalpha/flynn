//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/netguard"
)

// The inbound forward is the counterpart of governed egress: it lets a child in its own
// network namespace reach exactly one host-loopback service (the run's MCP bridge) that it
// otherwise could not, without opening the rest of the host loopback. These proofs use two
// loopback servers, one forwarded and one not, so they need no real bridge and cannot flake.

// forwardedSandbox is a Local whose child reaches the internet through nothing (deny-all
// egress) and reaches exactly bridgeAddr through the inbound forward. Egress is configured
// because the forward lives on the namespace the egress leg creates.
func forwardedSandbox(t *testing.T, bridgeAddr string) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir(), WithEgress(netguard.Policy{}), WithLoopbackForward(bridgeAddr))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// reachForward runs a probe that does a raw HTTP GET against the fixed in-namespace bridge
// address and returns what the child saw.
func reachForward(t *testing.T, sb *Local, name string) string {
	t.Helper()
	script := fmt.Sprintf(
		"exec 3<>/dev/tcp/127.0.0.1/%d || { echo FORWARD-UNREACHABLE; exit 1; }\n"+
			"printf 'GET / HTTP/1.0\\r\\nHost: 127.0.0.1:%d\\r\\n\\r\\n' >&3\n"+
			"cat <&3\n", netnsBridgePort, netnsBridgePort,
	)
	probe := egressProbe(t, sb.root, name, script)
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line})
	if err != nil {
		t.Fatalf("forward probe %s: %v", name, err)
	}
	return res.Output
}

// TestLoopbackForwardReachesTheBridge proves the forwarded bridge is reachable from inside
// the namespace at the fixed in-namespace address, so an external agent's MCP client can
// connect to the bridge the run hosts.
func TestLoopbackForwardReachesTheBridge(t *testing.T) {
	requireEgressHost(t)
	bridge := loopbackServer(t, "127.0.0.1", "REACHED-BRIDGE")
	sb := forwardedSandbox(t, bridge)

	if out := reachForward(t, sb, "reach.sh"); !strings.Contains(out, "REACHED-BRIDGE") {
		t.Fatalf("the forwarded bridge was not reachable inside the namespace; output:\n%s", out)
	}
}

// TestLoopbackForwardOpensOnlyTheForwardedAddress proves the forward opens exactly the one
// host address it was given: a second host-loopback service that was not forwarded stays
// unreachable, so forwarding the bridge does not quietly hand the child every local service.
func TestLoopbackForwardOpensOnlyTheForwardedAddress(t *testing.T) {
	requireEgressHost(t)
	bridge := loopbackServer(t, "127.0.0.1", "REACHED-BRIDGE")
	other := loopbackServer(t, "127.0.0.2", "REACHED-OTHER")
	sb := forwardedSandbox(t, bridge)

	// Control: the forward works, so a denial below is the forward's narrowness, not a dead launch.
	if out := reachForward(t, sb, "control.sh"); !strings.Contains(out, "REACHED-BRIDGE") {
		t.Fatalf("the forward is not working, so the denial below would prove nothing; output:\n%s", out)
	}

	ip, port, err := net.SplitHostPort(other)
	if err != nil {
		t.Fatal(err)
	}
	// The second server is live on the host, and the proxy's own namespace could reach it,
	// but the child was given a forward to the bridge only, so a direct dial has no route.
	probe := egressProbe(t, sb.root, "other.sh", "exec 3<>/dev/tcp/"+ip+"/"+port+" && echo REACHED-OTHER\n")
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line})
	if err != nil {
		t.Fatalf("other probe: %v", err)
	}
	if res.ExitCode == 0 || strings.Contains(res.Output, "REACHED-OTHER") {
		t.Fatalf("a host-loopback service that was not forwarded was reachable; output:\n%s", res.Output)
	}
}

// TestLoopbackForwardReleasesEachChildWhenItExits proves the per-child forwarder is let go
// as the command exits rather than accumulating, the same property the egress proxy has.
func TestLoopbackForwardReleasesEachChildWhenItExits(t *testing.T) {
	requireEgressHost(t)
	bridge := loopbackServer(t, "127.0.0.1", "REACHED-BRIDGE")
	sb := forwardedSandbox(t, bridge)

	for i := range 3 {
		if out := reachForward(t, sb, fmt.Sprintf("rel%d.sh", i)); !strings.Contains(out, "REACHED-BRIDGE") {
			t.Fatalf("round %d: forward not working:\n%s", i, out)
		}
		if live := sb.forward.liveChildren(); live != 0 {
			t.Fatalf("after %d finished commands, %d per-child forwarders are still held", i+1, live)
		}
	}
}

// TestForwardBridgeSwapsHostForTheNamespace is the unit half: on Linux the child cannot
// reach the host loopback, so ForwardBridge hands it the fixed in-namespace address and
// reports the host address as the forward target.
func TestForwardBridgeSwapsHostForTheNamespace(t *testing.T) {
	childURL, forwardTo := ForwardBridge("http://127.0.0.1:54321/mcp")
	if forwardTo != "127.0.0.1:54321" {
		t.Errorf("forwardTo = %q, want the host address 127.0.0.1:54321", forwardTo)
	}
	if !strings.Contains(childURL, "127.0.0.1:"+strconv.Itoa(netnsBridgePort)) {
		t.Errorf("childURL %q should point at the fixed in-namespace port %d", childURL, netnsBridgePort)
	}
	if !strings.HasSuffix(childURL, "/mcp") {
		t.Errorf("childURL %q should preserve the bridge path", childURL)
	}
	// A URL that does not parse is returned unchanged with no forward, so a bad address fails
	// loudly downstream rather than being silently rewritten.
	if c, f := ForwardBridge("://nope"); c != "://nope" || f != "" {
		t.Errorf("an unparseable URL should pass through unchanged, got (%q, %q)", c, f)
	}
}
