package sandbox

import (
	"errors"
	"fmt"
)

// ErrNetworkDeny reports [WithNetworkDenied] on a tier that cannot enforce it, the
// Windows write-restricted token. [WithBestEffortConfinement] never absorbs it;
// only [WithOpenNetworkWhereUndeniable] lets the command run.
var ErrNetworkDeny = errors.New("sandbox: cannot deny the network to the command")

// WithOpenNetworkWhereUndeniable runs with the host network where the tier cannot
// deny it, and [Local.NetworkDenied] then reports false. Where the tier can deny
// it ([NetworkDenySupported]) it changes nothing, so it is safe on every platform.
func WithOpenNetworkWhereUndeniable() LocalOption {
	return func(l *Local) { l.networkOpenOK = true }
}

// NetworkDenySupported reports it for [WithDefaultConfinement]: false on Windows,
// whose default token has no network control; only the AppContainer tier
// ([WithKernelConfinement]) denies it there.
func NetworkDenySupported() bool { return networkDenyEnforceable(true) }

// networkDenied is shared by [Local.NetworkDenied] and the launch guard, so a
// record never claims a denial that was not enforced.
func (l *Local) networkDenied() bool {
	return l.denyNetwork && networkDenyEnforceable(l.hostReadable)
}

// guardNetworkDeny honours [WithOpenNetworkWhereUndeniable].
func (l *Local) guardNetworkDeny() error {
	if !l.denyNetwork || networkDenyEnforceable(l.hostReadable) || l.networkOpenOK {
		return nil
	}
	return fmt.Errorf("%w: this confinement tier has no network control; pass WithOpenNetworkWhereUndeniable to run with the host's network, or use the AppContainer tier", ErrNetworkDeny)
}
