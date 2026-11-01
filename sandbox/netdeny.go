package sandbox

import (
	"errors"
	"fmt"
)

// ErrNetworkDeny reports that a Local asked for no network ([WithNetworkDenied]) on a
// confinement tier that cannot enforce it: the write-restricted token that
// [WithDefaultConfinement] selects on Windows. It fails the launch, and
// [WithBestEffortConfinement] never absorbs it, because a command that was to have no
// network and ran with the host's is the failure the request exists to prevent.
// [WithOpenNetworkWhereUndeniable] is the explicit acceptance that lets the command run
// with the network open.
var ErrNetworkDeny = errors.New("sandbox: cannot deny the network to the command")

// WithOpenNetworkWhereUndeniable accepts running with the host's network on a tier that
// cannot deny it. A Local that asked for [WithNetworkDenied] on such a tier refuses every
// launch with [ErrNetworkDeny]; with this option it runs, and [Local.NetworkDenied] then
// reports false so a record built from the Local says the network was open. It changes
// nothing where the tier can deny the network (see [NetworkDenySupported]), so a caller
// can set it on every platform and still get the denial where there is one.
func WithOpenNetworkWhereUndeniable() LocalOption {
	return func(l *Local) { l.networkOpenOK = true }
}

// NetworkDenySupported reports whether the confinement [WithDefaultConfinement] selects
// can deny the network to a command. It is true on Linux and macOS and false on Windows,
// whose write-restricted token has no network control and which denies the network only
// under the AppContainer tier (see [WithKernelConfinement]) that the default does not use.
// A caller can choose what to do there before a launch refuses.
func NetworkDenySupported() bool { return networkDenyEnforceable(true) }

// networkDenied reports whether commands run with no network: it was asked for and this
// Local's tier enforces it. The one answer [Local.NetworkDenied] and the launch guard share,
// so a record never claims a denial the guard let through unenforced.
func (l *Local) networkDenied() bool {
	return l.denyNetwork && networkDenyEnforceable(l.hostReadable)
}

// guardNetworkDeny refuses a launch that asked for no network on a tier that cannot
// provide it, unless the Local accepted the open network.
func (l *Local) guardNetworkDeny() error {
	if !l.denyNetwork || networkDenyEnforceable(l.hostReadable) || l.networkOpenOK {
		return nil
	}
	return fmt.Errorf("%w: this confinement tier has no network control; pass WithOpenNetworkWhereUndeniable to run with the host's network, or use the AppContainer tier", ErrNetworkDeny)
}
