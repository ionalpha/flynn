package sandbox

import "sync"

// forwardConfig forwards one host-loopback address into a child's namespace, so the
// child reaches the run's MCP bridge and nothing else on host loopback.
type forwardConfig struct {
	// hostAddr is host:port on the host loopback.
	hostAddr string

	mu sync.Mutex
	// perChild holds live forwarder teardowns keyed by launch. One forwarder serves
	// one child because a namespace's loopback is reachable only from inside it.
	perChild map[any]func()
}

// WithLoopbackForward makes hostAddr reachable at a fixed loopback address inside
// the child's namespace, and no other host-loopback service. On Linux it needs
// [WithEgress], which creates the namespace. On macOS the child shares the host
// stack, so ForwardBridge reports no forward is needed.
func WithLoopbackForward(hostAddr string) LocalOption {
	return func(l *Local) { l.forward = &forwardConfig{hostAddr: hostAddr} }
}

// close is idempotent.
func (f *forwardConfig) close() {
	f.mu.Lock()
	live := f.perChild
	f.perChild = nil
	f.mu.Unlock()
	for _, release := range live {
		release()
	}
}
