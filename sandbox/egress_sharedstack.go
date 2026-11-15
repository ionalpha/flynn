//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/netguard"
)

// attachEgress sets the proxy variables for a child on the host stack. The child
// may ignore them; confine enforces the proxy by denying other egress (seatbelt on
// macOS). One proxy serves every child until Close, so the release is a no-op.
func (l *Local) attachEgress(c *exec.Cmd) (func(), error) {
	addr, err := l.egress.ensureProxy()
	if err != nil {
		return func() {}, err
	}
	c.Env = mergeEnv(c.Env, proxyEnvVars(addr))
	return func() {}, nil
}

// ensureProxy starts the loopback proxy on first call and returns its address.
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
