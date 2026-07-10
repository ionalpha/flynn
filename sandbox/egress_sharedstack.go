//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/netguard"
)

// attachEgress points a child that shares the host's network stack at the sandbox's
// proxy: the proxy listens on the host's loopback, and the standard proxy variables in
// the child's environment name it. One proxy serves every child, started on first use.
//
// This is only half of the enforcement. The child's environment is advice it could
// ignore; what makes the proxy the single way out is the platform's confine denying the
// child's direct egress (on macOS, a seatbelt profile that allows outbound only to the
// proxy's address). On a platform with no such leg, egressEnforceable is false and the
// launch has already refused before reaching here.
// The one proxy is shared by every child and lives as long as the Local, so a launch here
// holds nothing of its own and its release is a no-op. Close stops the proxy.
func (l *Local) attachEgress(c *exec.Cmd) (func(), error) {
	addr, err := l.egress.ensureProxy()
	if err != nil {
		return func() {}, err
	}
	c.Env = mergeEnv(c.Env, proxyEnvVars(addr))
	return func() {}, nil
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
