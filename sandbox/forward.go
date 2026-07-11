package sandbox

import "sync"

// forwardConfig is the inbound counterpart of egressConfig: it forwards exactly one
// host-loopback address into a confined child's network namespace, so the child can reach
// that one host service (the run's MCP bridge) and nothing else on the host loopback. On a
// platform whose child shares the host's network stack there is nothing to forward: the
// child reaches the address directly, and ForwardBridge reports so, so this is never
// configured there.
type forwardConfig struct {
	// hostAddr is the single host-loopback address (host:port) connections to the
	// in-namespace listener are piped to.
	hostAddr string

	mu sync.Mutex
	// perChild holds the teardown of every forwarder that serves a single child and is
	// still live, keyed by the launch that owns it. One forwarder serves one child, because
	// a namespace's loopback is reachable only from inside it. A launch releases its own
	// forwarder when its child exits and drops itself from here; what remains at close is
	// what never got that far.
	perChild map[any]func()
}

// WithLoopbackForward forwards one host-loopback address into the confined child's network
// namespace, reachable from the child at a fixed in-namespace loopback address. It is the
// inbound counterpart of WithEgress and, on Linux, depends on it: the forward lives on the
// same network namespace the egress leg creates. The child reaches exactly the one host
// address named here and nothing else on the host loopback, so the run's own MCP bridge is
// reachable while every other local service on the box is not.
//
// On a platform whose child shares the host's network stack (macOS) the child reaches the
// host loopback directly, so ForwardBridge reports no forward is needed and this option is
// not configured. It exists on every platform so callers compile uniformly.
func WithLoopbackForward(hostAddr string) LocalOption {
	return func(l *Local) { l.forward = &forwardConfig{hostAddr: hostAddr} }
}

// close releases every per-child forwarder still registered. It is idempotent.
func (f *forwardConfig) close() {
	f.mu.Lock()
	live := f.perChild
	f.perChild = nil
	f.mu.Unlock()
	for _, release := range live {
		release()
	}
}
