// Package bindguard is the inbound network policy every Flynn listener binds
// through, the mirror of netguard's egress gate. A listener binds loopback unless an
// operator opts into wider exposure. A wildcard bind (0.0.0.0, ::, or an empty host,
// which binds interfaces the operator may not know about) is always refused, so
// even granted exposure must name an interface. Reaching a service from another
// machine goes through a tunnel to its loopback bind.
//
// A host given as a name is resolved and every address must pass, so a name that
// resolves off-host is never bound.
package bindguard

import (
	"net"
	"net/netip"

	"github.com/ionalpha/flynn/fault"
)

// Exposure decides how widely a listener may bind. The zero Exposure is
// loopback-only. AllowNonLoopback also permits a named non-loopback unicast
// address, never a wildcard.
//
//flynn:policy
type Exposure struct {
	// AllowNonLoopback is the operator opt-in to bind a named non-loopback unicast
	// address (a chosen LAN or public IP). It never permits a wildcard bind.
	AllowNonLoopback bool
}

// Loopback is the default exposure: only the loopback interface may be bound.
func Loopback() Exposure { return Exposure{} }

// Exposed permits binding a named non-loopback address, never a wildcard. Use it
// only for an audited exposure; a tunnel to a loopback bind is preferred.
func Exposed() Exposure { return Exposure{AllowNonLoopback: true} }

func refused(msg string) error {
	return fault.New(fault.Forbidden, "bind_denied", "bindguard: "+msg)
}

// CheckHost reports whether host, the host part of a listen address, may be bound
// under e. An empty or unspecified host (0.0.0.0, ::) is a wildcard and always
// refused; loopback is always allowed; any other address needs e.AllowNonLoopback.
// A name is resolved and every address must pass.
func CheckHost(host string, e Exposure) error {
	if host == "" {
		return refused("refusing to bind a wildcard address (every interface); bind a specific address, loopback recommended")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return checkAddr(addr, e)
	}
	// Every resolved address must pass, so a name mapping to a non-loopback or
	// wildcard address is not bound off-host.
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

// checkAddr applies the policy to one parsed address.
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

// Listen binds a listener on addr (host:port) for network under e. It is the one
// place a TCP listener is opened, so no bind bypasses the policy, and the address
// is checked first, so an unsafe one fails before a socket opens.
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

// FreeLoopbackPort binds loopback port 0, reads the assigned port and releases it
// for a server to claim. The gap before the real bind is the accepted cost of
// choosing a port this way. It goes through the loopback policy, so discovery
// cannot bind off-loopback.
func FreeLoopbackPort() (int, error) {
	ln, err := Listen("tcp", "127.0.0.1:0", Loopback())
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
