//go:build !linux

package sandbox

import "os/exec"

// ForwardBridge reports that a child sharing the host's network stack reaches the bridge
// directly, so there is no forward to set up and the child uses the host URL unchanged. On
// macOS the seatbelt egress profile permits the bridge's loopback address; elsewhere the
// caller has already refused a governed launch before reaching here.
func ForwardBridge(hostURL string) (childURL, forwardTo string) {
	return hostURL, ""
}

// attachLoopbackForward is a no-op where the child shares the host's network stack: the
// bridge is reachable directly and WithLoopbackForward is never configured (ForwardBridge
// reports no forward). It exists so the shared startEgress compiles on every platform.
func (l *Local) attachLoopbackForward(_ *exec.Cmd) (func(), error) {
	return func() {}, nil
}
