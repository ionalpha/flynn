package sandbox

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/bindguard"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// errEgressUnsupported is returned when a governed-egress launch is requested on a
// platform whose enforcement leg is not present. The caller refuses the launch rather
// than running the child with its direct egress open, so a missing leg fails closed. It
// is a governance refusal (Forbidden), like the other confinement-unsupported refusals.
var errEgressUnsupported = fault.New(fault.Forbidden, "sandbox_egress_unsupported",
	"sandbox: governed egress is not enforceable on this platform yet; refusing rather than running with the child's direct egress open")

// egressConfig is the outbound policy for the children a Local launches. When set, a
// child is launched with its direct egress denied at the OS level and pointed at a
// loopback proxy that admits only what the policy allows, so the one egress policy
// engine (netguard) governs a process whose own code we do not control, and the child
// cannot bypass it.
type egressConfig struct {
	policy netguard.Policy

	mu    sync.Mutex
	proxy net.Listener // the loopback listener the proxy serves on; started lazily
	addr  string       // the proxy's address, for HTTP(S)_PROXY in the child env
	stop  context.CancelFunc
}

// WithEgress governs the outbound network of every child the sandbox launches through
// policy: the child is pointed at a loopback proxy that enforces policy, and its direct
// egress is denied at the OS level so the proxy is the only way out. It is the OS-level
// reuse of the same netguard policy that guards the agent's own dials. On a platform
// whose enforcement leg is not present, a launch under this option refuses rather than
// running with the network silently open (refuse-rather-than-weaken), exactly as
// WithNetworkDenied does.
func WithEgress(policy netguard.Policy) LocalOption {
	return func(l *Local) { l.egress = &egressConfig{policy: policy} }
}

// ensureProxy starts the egress proxy once, on a loopback listener, and returns its
// address. Subsequent calls return the running proxy's address. The proxy lives until
// the Local is closed.
func (e *egressConfig) ensureProxy() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.proxy != nil {
		return e.addr, nil
	}
	ln, err := bindguard.Listen("tcp", "127.0.0.1:0", bindguard.Loopback())
	if err != nil {
		return "", fmt.Errorf("sandbox: egress proxy listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	px := netguard.NewProxy(e.policy)
	go func() { _ = px.Serve(ctx, ln) }()
	e.proxy = ln
	e.addr = ln.Addr().String()
	e.stop = cancel
	return e.addr, nil
}

// close stops the proxy if it is running. It is idempotent.
func (e *egressConfig) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stop != nil {
		e.stop()
		e.stop = nil
	}
	if e.proxy != nil {
		_ = e.proxy.Close()
		e.proxy = nil
	}
}

// proxyEnvVars returns the environment that points a child at the egress proxy: the
// standard proxy variables (upper and lower case, since tools differ) and a NO_PROXY
// that still allows the child to reach its own loopback so a co-located service it talks
// to locally is not forced through the proxy.
func proxyEnvVars(addr string) map[string]string {
	url := "http://" + addr
	return map[string]string{
		"HTTP_PROXY":  url,
		"HTTPS_PROXY": url,
		"ALL_PROXY":   url,
		"http_proxy":  url,
		"https_proxy": url,
		"all_proxy":   url,
		"NO_PROXY":    "localhost,127.0.0.1,::1",
		"no_proxy":    "localhost,127.0.0.1,::1",
	}
}

// startEgress starts the egress proxy (once) and injects the proxy variables into c's
// environment so the child routes its outbound through the proxy. The OS-level denial of
// the child's direct egress is applied by the platform's confine, which reads the proxy
// address from the egress config; egress and confinement compose into one enforcement
// action (one seatbelt profile, one network namespace) rather than two independent
// wrappings. A nil egress config is a no-op.
//
// The caller gates this on egressEnforceable: a launch with egress requested on a
// platform whose enforcement leg is not present refuses (errEgressUnsupported) before
// reaching here, so the proxy env is never injected without the OS-level denial behind
// it (which would be cooperative-only, i.e. bypassable).
func (l *Local) startEgress(c *exec.Cmd) error {
	if l.egress == nil {
		return nil
	}
	addr, err := l.egress.ensureProxy()
	if err != nil {
		return err
	}
	c.Env = mergeEnv(c.Env, proxyEnvVars(addr))
	return nil
}

// guardEgress refuses a governed-egress launch on a platform whose enforcement leg is
// not present, so egress fails closed (refuse-rather-than-weaken) rather than running
// the child with its direct egress open. It is called at the launch entry points (Exec,
// Serve) before any dispatch, since the Windows command path does not run through
// confine.
func (l *Local) guardEgress() error {
	if l.egress != nil && !egressEnforceable() {
		return errEgressUnsupported
	}
	return nil
}

// mergeEnv overlays vars onto a KEY=VALUE environment, replacing any existing entry for
// the same key, and returns it sorted (stable for tests and logs).
func mergeEnv(env []string, vars map[string]string) []string {
	merged := make(map[string]string, len(env)+len(vars))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for k, v := range vars {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// egressActive reports whether this Local governs child egress, so the launch paths
// know to call applyEgress and treat the launch as one that must be confined.
func (l *Local) egressActive() bool { return l.egress != nil }
