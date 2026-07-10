package sandbox

import (
	"context"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"

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
	// addr is the proxy's address, for HTTP(S)_PROXY in the child env and for the seatbelt
	// rule that allows only it. Only a platform whose children share the host's network
	// stack has an address to name: a Linux child's proxy endpoint lives on its own
	// namespace's loopback and is never referred to from out here.
	addr string //nolint:unused // read by the shared-network-stack leg; see attachEgress
	stop context.CancelFunc

	// perChild holds the teardown of every proxy that serves a single child and is still
	// live, keyed by the launch that owns it. A platform whose child gets a private
	// network stack (Linux) needs one proxy per child, since a namespace's loopback is
	// reachable only from inside it; a platform whose child shares the host stack (macOS)
	// leaves this empty and reuses the single lazily-started proxy above. It is a
	// backstop, not the primary path: a launch releases its own proxy when its child
	// exits, and drops itself from here. What remains at close is what never got that
	// far, so a Local that ran ten thousand commands holds nothing for the ones that
	// finished.
	perChild map[any]func()
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

// close stops the proxy if it is running, and releases every per-child proxy. It is
// idempotent.
func (e *egressConfig) close() {
	e.mu.Lock()
	if e.stop != nil {
		e.stop()
		e.stop = nil
	}
	if e.proxy != nil {
		_ = e.proxy.Close()
		e.proxy = nil
	}
	// A release drops itself from perChild and so takes this lock. The map is detached
	// here and the releases run below, unlocked, rather than deadlocking against it.
	live := e.perChild
	e.perChild = nil
	e.mu.Unlock()

	for _, release := range live {
		release()
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

// startEgress prepares c to run with its outbound traffic governed by the proxy. The
// OS-level denial of the child's direct egress is applied by the platform's confine, so
// egress and confinement compose into one enforcement action (one seatbelt profile, one
// network namespace) rather than two independent wrappings. A nil egress config is a
// no-op.
//
// How the child reaches the proxy is the platform's business, because where the proxy
// can live differs: on macOS the child shares the host's network stack, so the proxy
// listens on the host's loopback and the seatbelt profile allows only that address. On
// Linux the child gets its own network namespace with its own loopback, which the host's
// proxy cannot reach, so the listening socket is created inside the namespace and handed
// back out. attachEgress carries that difference; everything above it is shared.
//
// The caller gates this on egressEnforceable: a launch with egress requested on a
// platform whose enforcement leg is not present refuses (errEgressUnsupported) before
// reaching here, so the proxy env is never injected without the OS-level denial behind
// it (which would be cooperative-only, i.e. bypassable).
//
// The returned release frees whatever this launch holds, and must be called when the
// child is done: after the command exits for a run-to-completion launch, and when the
// process is reaped for a backgrounded one. On a platform that gives each child its own
// proxy, a Local that launched many children would otherwise hold one proxy per launch
// until it closed. It is idempotent, and Close calls it for any launch that did not.
func (l *Local) startEgress(c *exec.Cmd) (release func(), err error) {
	if l.egress == nil {
		return func() {}, nil
	}
	return l.attachEgress(c)
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
