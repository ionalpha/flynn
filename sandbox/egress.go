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

// errEgressUnsupported refuses a governed-egress launch on a platform with no
// enforcement leg, so a missing leg fails closed.
var errEgressUnsupported = fault.New(fault.Forbidden, "sandbox_egress_unsupported",
	"sandbox: governed egress is not enforceable on this platform yet; refusing rather than running with the child's direct egress open")

// egressConfig denies a child's direct egress at the OS level and points it at a
// loopback proxy enforcing the netguard policy, so code we do not control cannot
// bypass the policy.
type egressConfig struct {
	policy netguard.Policy

	mu    sync.Mutex
	proxy net.Listener // started lazily
	// addr feeds HTTP(S)_PROXY and the seatbelt allow rule. It is unused on Linux,
	// where the proxy endpoint is inside the child's namespace.
	addr string //nolint:unused // read by the shared-network-stack leg; see attachEgress
	stop context.CancelFunc

	// perChild holds live per-child proxy teardowns, keyed by launch. Linux needs
	// one proxy per child because a namespace's loopback is reachable only from
	// inside; macOS shares the host stack and leaves this empty. A launch removes
	// its entry when its child exits, so close releases only launches that died.
	perChild map[any]func()
}

// GovernedEgressAvailable is true on Linux, macOS and WSL2, so a caller can refuse
// with an actionable message before a launch hits the fail-closed refusal.
func GovernedEgressAvailable() bool { return egressEnforceable() }

// WithEgress makes a policy-enforcing loopback proxy every child's only way out.
// Where the platform cannot enforce it, launches refuse, as with WithNetworkDenied.
func WithEgress(policy netguard.Policy) LocalOption {
	return func(l *Local) { l.egress = &egressConfig{policy: policy} }
}

// close is idempotent.
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
	// A release takes this lock to drop itself, so the releases run unlocked.
	live := e.perChild
	e.perChild = nil
	e.mu.Unlock()

	for _, release := range live {
		release()
	}
}

// proxyEnvVars sets both cases because tools differ, and exempts loopback so a
// co-located service is not forced through the proxy.
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

// startEgress attaches the proxy; confine applies the matching OS denial. The
// caller checks egressEnforceable first, because proxy variables without the
// denial are bypassable. The release must be called when the child is done, or
// the Local holds a proxy per launch until Close.
func (l *Local) startEgress(c *exec.Cmd) (release func(), err error) {
	var releases []func()
	runReleases := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	if l.egress != nil {
		r, err := l.attachEgress(c)
		if err != nil {
			return func() {}, err
		}
		releases = append(releases, r)
	}
	// The inbound forward exposes one host-loopback address (the MCP bridge) and
	// shares the namespace egress set up.
	if l.forward != nil {
		r, err := l.attachLoopbackForward(c)
		if err != nil {
			runReleases()
			return func() {}, err
		}
		releases = append(releases, r)
	}
	if len(releases) == 0 {
		return func() {}, nil
	}
	return runReleases, nil
}

// guardEgress runs at the launch entry points, because the Windows command path
// does not run through confine.
func (l *Local) guardEgress() error {
	if l.egress != nil && !egressEnforceable() {
		return errEgressUnsupported
	}
	return nil
}

// mergeEnv returns the result sorted, so tests and logs are stable.
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

// egressActive means the launch must call startEgress and be confined.
func (l *Local) egressActive() bool { return l.egress != nil }
