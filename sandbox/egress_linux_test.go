//go:build linux

package sandbox

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/netguard"
)

// The governed-egress leg is proven here against the real adapter, hermetically: two
// servers on two loopback addresses stand in for an allowed and a denied destination, so
// the proofs need no internet and cannot flake on one. What is asserted is the property
// the containment promise rests on, which is not "the child prefers the proxy" but "the
// child has no other way out":
//
//   - a destination the policy allows is reachable through the proxy;
//   - a destination the policy denies is refused, though it sits on an address the child
//     would reach if it could dial for itself;
//   - the host's loopback is not the child's loopback, so a service the host runs locally
//     is not reachable just by being local;
//   - a direct dial to a routable address has no interface to leave through.
//
// Together those say the proxy is the only route, and the policy governs it.

// egressProbe writes a bash probe into the sandbox root and returns the command that runs
// it. Scripts go through a file rather than a quoted -c string so the shell quoting of the
// probe cannot change what the launch path is being asked to prove.
func egressProbe(t *testing.T, root, name, script string) Command {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test probe in a temp dir
		t.Fatalf("write probe: %v", err)
	}
	return Command{Line: "bash ./" + name}
}

// requireEgressHost skips when this host cannot run the leg at all, for a reason that is
// the host's rather than the code's: the probes need bash, and the launcher needs to
// perform privileged setup inside its user namespace.
//
// Creating the namespace is not enough to go on. Ubuntu 24.04 and its CI runner let an
// unprivileged CLONE_NEWUSER succeed and then deny the capabilities inside it, so the
// launcher's mount and its SIOCSIFFLAGS both come back EPERM. This probe therefore asks
// for a read-only filesystem, whose setup needs the same in-namespace privilege that
// raising loopback does, by a different mechanism. Where that cannot be established,
// governed egress cannot be either, and these tests would be reporting on the host rather
// than on the code. Where it can, an egress failure below is ours.
func requireEgressHost(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed; the egress probes need it")
	}
	l, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	defer func() { _ = l.Close() }()
	// The launcher reports a setup failure as a non-zero exit; a namespace that could not
	// be created at all is the error path.
	res, err := l.Exec(context.Background(), Command{Line: "true"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("probing confinement support: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("this host denies the launcher's privileged setup inside a user namespace, so governed egress cannot be established here: %s", strings.TrimSpace(res.Output))
	}
}

// loopbackServer starts an HTTP server bound to addr's IP that answers with body. 127/8
// is entirely loopback on Linux, so two of them give two distinct destinations that are
// both reachable from the host and both unreachable from a fresh network namespace.
func loopbackServer(t *testing.T, ip, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}

// governedSandbox is a Local whose children may reach only allowed, through the proxy.
func governedSandbox(t *testing.T, allowed ...netip.Prefix) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir(), WithEgress(netguard.Policy{Allow: allowed}))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// proxyGet is a probe that fetches an absolute URL through whatever proxy the sandbox put
// in the child's environment. It proves the endpoint the launcher created inside the
// namespace is real, is served, and is governed.
const proxyGet = `set -u
target="$1"
proxy="${HTTP_PROXY#http://}"
exec 3<>/dev/tcp/${proxy%%:*}/${proxy##*:} || { echo "PROXY-UNREACHABLE"; exit 1; }
printf 'GET http://%s/ HTTP/1.0\r\nHost: %s\r\n\r\n' "$target" "$target" >&3
cat <&3
`

// fetchThroughProxy runs the proxy-GET probe against target and returns what the child
// saw. The probe name is per-call so several can share one sandbox root.
func fetchThroughProxy(t *testing.T, sb *Local, name, target string) string {
	t.Helper()
	probe := egressProbe(t, sb.root, name, proxyGet+"\n")
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line + " " + target})
	if err != nil {
		t.Fatalf("probe %s: %v", name, err)
	}
	return res.Output
}

// requireProxyWorks is the control every denial proof needs: it asserts the child ran, the
// launcher built the endpoint, and the proxy serves an allowed destination through it. A
// launch that dies on its own (a failed namespace setup, a missing bash) makes every
// escape probe fail too, which would let a denial assertion pass while proving nothing.
func requireProxyWorks(t *testing.T, sb *Local, allowed string) {
	t.Helper()
	out := fetchThroughProxy(t, sb, "control.sh", allowed)
	if !strings.Contains(out, "REACHED-ALLOWED") {
		t.Fatalf("the governed launch is not working, so a denial here would prove nothing; control output:\n%s", out)
	}
}

func TestGovernedEgressReachesAnAllowedHostThroughTheProxy(t *testing.T) {
	requireEgressHost(t)
	allowed := loopbackServer(t, "127.0.0.1", "REACHED-ALLOWED")
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))

	if out := fetchThroughProxy(t, sb, "allowed.sh", allowed); !strings.Contains(out, "REACHED-ALLOWED") {
		t.Fatalf("an allowed host was not reachable through the proxy; output:\n%s", out)
	}
}

func TestGovernedEgressRefusesADeniedHostThroughTheProxy(t *testing.T) {
	requireEgressHost(t)
	allowed := loopbackServer(t, "127.0.0.1", "REACHED-ALLOWED")
	denied := loopbackServer(t, "127.0.0.2", "REACHED-DENIED")
	// The policy allows 127.0.0.1 only, so the server on 127.0.0.2 is a destination the
	// child can name but the policy refuses. Without the policy check it would be reached:
	// the proxy runs in the host's namespace, where that address is live.
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))
	requireProxyWorks(t, sb, allowed)

	if out := fetchThroughProxy(t, sb, "denied.sh", denied); strings.Contains(out, "REACHED-DENIED") {
		t.Fatalf("the proxy served a destination the policy denies; output:\n%s", out)
	}
}

// The child's loopback is its namespace's own, not the host's. A service the host runs on
// 127.0.0.1 is therefore not reachable from the child by virtue of being local, which is
// what stops governed egress from quietly handing the child every loopback service on the
// box (including the agent's own).
func TestGovernedEgressChildCannotDialTheHostsLoopbackDirectly(t *testing.T) {
	requireEgressHost(t)
	host := loopbackServer(t, "127.0.0.1", "REACHED-ALLOWED")
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))
	requireProxyWorks(t, sb, host)

	ip, port, err := net.SplitHostPort(host)
	if err != nil {
		t.Fatal(err)
	}
	// Unconfined, this dial succeeds: the server is listening. Confined, the only thing on
	// the child's loopback is the proxy endpoint, so the connect must fail. The proxy
	// itself reaches this server (the test above), which rules out the server being down.
	probe := egressProbe(t, sb.root, "hostloop.sh",
		"exec 3<>/dev/tcp/"+ip+"/"+port+" && echo REACHED-HOST-LOOPBACK\n")
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.ExitCode == 0 || strings.Contains(res.Output, "REACHED-HOST-LOOPBACK") {
		t.Fatalf("the child reached a host loopback service directly; output:\n%s", res.Output)
	}
}

// A direct dial to a routable address has no interface to leave through: the namespace
// holds only loopback and carries no route. This is the property the whole leg rests on,
// so it is asserted against the error the kernel actually gives rather than against a
// bare non-zero exit, which a host with no outbound network would also produce.
func TestGovernedEgressChildHasNoRouteForADirectDial(t *testing.T) {
	requireEgressHost(t)
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))

	probe := egressProbe(t, sb.root, "direct.sh", "exec 3<>/dev/tcp/8.8.8.8/53\n")
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a direct dial to a routable address succeeded; the child is not contained:\n%s", res.Output)
	}
	// "Network is unreachable" is the namespace having no route, which is containment. A
	// timeout or a refusal would mean the dial left the namespace and failed elsewhere.
	if !strings.Contains(res.Output, "unreachable") {
		t.Fatalf("direct dial failed, but not for want of a route, so containment is unproven:\n%s", res.Output)
	}
}

// Each governed child gets a proxy of its own, so a sandbox that runs many commands must
// let each one go as its command exits rather than accumulating them until it closes. A
// long-lived agent session runs thousands of commands through one sandbox.
func TestGovernedEgressReleasesEachChildsProxyWhenItExits(t *testing.T) {
	requireEgressHost(t)
	allowed := loopbackServer(t, "127.0.0.1", "REACHED-ALLOWED")
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))

	for i := range 3 {
		requireProxyWorks(t, sb, allowed)
		if live := sb.egress.liveChildren(); live != 0 {
			t.Fatalf("after %d finished commands, %d per-child proxies are still held", i+1, live)
		}
	}
}

// The launcher's half in isolation: a fresh namespace's loopback starts down, and nothing
// (not even 127.0.0.1) is reachable on it until it is raised. If this regressed, the proxy
// endpoint would be created on an interface no child could connect to, and every governed
// run would fail closed rather than run.
func TestLoopbackComesUpInsideTheNamespace(t *testing.T) {
	requireEgressHost(t)
	sb := governedSandbox(t, netip.MustParsePrefix("127.0.0.1/32"))
	probe := egressProbe(t, sb.root, "lo.sh", "ip link show lo 2>/dev/null | head -1\n")
	res, err := sb.Exec(context.Background(), Command{Line: "timeout 10 " + probe.Line})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if strings.TrimSpace(res.Output) == "" {
		t.Skip("iproute2 is not installed; the loopback state is proven indirectly by the proxy probes")
	}
	if !strings.Contains(res.Output, "UP") {
		t.Fatalf("loopback is not up inside the namespace:\n%s", res.Output)
	}
}
