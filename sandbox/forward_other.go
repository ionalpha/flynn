//go:build !linux

package sandbox

import "os/exec"

// ForwardBridge returns hostURL unchanged and no forward, because the child shares
// the host stack. The macOS seatbelt profile permits the bridge address.
func ForwardBridge(hostURL string) (childURL, forwardTo string) {
	return hostURL, ""
}

// attachLoopbackForward is a no-op, because the bridge is reachable directly.
func (l *Local) attachLoopbackForward(_ *exec.Cmd) (func(), error) {
	return func() {}, nil
}
