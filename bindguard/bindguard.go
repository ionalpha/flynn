// Package bindguard is the inbound network policy every listener Flynn opens is
// bound through: a default-loopback gate that decides whether a given listen address
// may be bound. It is the inbound mirror of netguard (the outbound egress gate): where
// netguard stops a connection the agent makes from reaching somewhere it should not,
// bindguard stops a listener the agent opens from being reachable by someone it should
// not.
//
// The doctrine is bind-safe by default. A listener binds the loopback interface unless
// an operator explicitly opts into wider exposure, and a wildcard bind (0.0.0.0, ::, or
// an empty host, which binds every interface including ones the operator does not know
// about) is refused unconditionally: even when exposure is granted, a specific interface
// address must be named. The recommended way to reach a service from off the machine is a
// tunnel to its loopback bind, not a public bind, so the safe default needs no extra
// thought and the unsafe shape needs an explicit, auditable choice.
//
// The gate is enforced at the point of bind. A host given as a name is resolved and every
// resolved address must pass, so a name that resolves to a non-loopback address is not
// silently bound off-host.
package bindguard

import (
	"net"
	"net/netip"

	"github.com/ionalpha/flynn/fault"
)

// Exposure decides how widely a listener may bind. The zero Exposure is loopback-only
// (bind-safe by default): a bind is allowed only on the loopback interface. Granting
// AllowNonLoopback additionally permits a named non-loopback unicast interface address,
// but never a wildcard bind.
type Exposure struct {
	// AllowNonLoopback permits binding a specific, non-loopback unicast interface
	// address (a chosen LAN or public IP). It is the explicit operator opt-in for
	// exposing a service off the loopback interface. It never permits a wildcard bind:
	// a specific address must always be named.
	AllowNonLoopback bool
}

// Loopback is the bind-safe default: only the loopback interface may be bound.
func Loopback() Exposure { return Exposure{} }

// Exposed permits binding a named non-loopback interface address (still never a
// wildcard). Use it only for a deliberate, audited exposure; prefer a tunnel to a
// loopback bind.
func Exposed() Exposure { return Exposure{AllowNonLoopback: true} }

func refused(msg string) error {
	return fault.New(fault.Forbidden, "bind_denied", "bindguard: "+msg)
}

// CheckHost reports whether host (the host part of a listen address) may be bound under
// e. An empty host, or a host that parses to the unspecified address (0.0.0.0 or ::), is
// a wildcard bind and is always refused. A loopback host is always allowed. Any other
// address is allowed only when e.AllowNonLoopback is set. A non-IP host is resolved and
// every resolved address must pass.
func CheckHost(host string, e Exposure) error {
	if host == "" {
		return refused("refusing to bind a wildcard address (every interface); bind a specific address, loopback recommended")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return checkAddr(addr, e)
	}
	// A name: resolve it and require every resolved address to pass, so a name that
	// maps to a non-loopback (or wildcard-equivalent) address is not bound off-host.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return refused("cannot resolve bind host " + host + ": " + err.Error())
	}
	if len(addrs) == 0 {
		return refused("bind host " + host + " resolved to no address")
	}
	for _, a := range addrs {
		addr, perr := netip.ParseAddr(a)
		if perr != nil {
			return refused("bind host " + host + " resolved to an unparseable address " + a)
		}
		if cerr := checkAddr(addr, e); cerr != nil {
			return cerr
		}
	}
	return nil
}

// checkAddr applies the doctrine to a single parsed address.
func checkAddr(addr netip.Addr, e Exposure) error {
	if !addr.IsValid() {
		return refused("invalid bind address")
	}
	addr = addr.Unmap() // treat an IPv4-in-IPv6 address as IPv4
	if addr.IsUnspecified() {
		return refused("refusing to bind the wildcard address " + addr.String() + " (every interface); bind a specific address, loopback recommended")
	}
	if addr.IsMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsLinkLocalMulticast() {
		return refused("refusing to bind a multicast address " + addr.String())
	}
	if addr.IsLoopback() {
		return nil
	}
	if !e.AllowNonLoopback {
		return refused("refusing to bind non-loopback address " + addr.String() + " by default; bind loopback and reach it through a tunnel, or pass the explicit exposure opt-in")
	}
	return nil
}

// Listen binds a listener on addr (host:port) for network, enforcing e. It is the
// governed counterpart of the standard net.Listen: the one place a TCP listener is
// opened, so a new bind cannot bypass the inbound policy. The address is checked before
// the bind, so an unsafe address fails closed rather than opening a socket.
func Listen(network, addr string, e Exposure) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, refused("cannot parse listen address " + addr + ": " + err.Error())
	}
	if cerr := CheckHost(host, e); cerr != nil {
		return nil, cerr
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// FreeLoopbackPort asks the OS for an unused loopback TCP port by binding port 0 and
// reading back the assignment, then releasing it for a server to claim. The brief gap
// between release and the real bind is the standard, accepted way to choose a port. It
// goes through the loopback policy so even free-port discovery cannot bind off-loopback.
func FreeLoopbackPort() (int, error) {
	ln, err := Listen("tcp", "127.0.0.1:0", Loopback())
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
