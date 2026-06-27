// Package netguard is the outbound network policy the agent dials its own
// connections through: a default-deny gate that decides whether a given address may
// be reached. It is the in-process half of egress control, the layer that stops a
// connection the agent itself makes from going somewhere it should not, including a
// server-side request forgery to a private or cloud-metadata address.
//
// One Policy type expresses the range of needs: a sandboxed step that may reach
// nothing (the zero value denies everything), a download that may reach any public
// host but never a private or loopback one, and a run granted a specific range. The
// gate is enforced at the point of connect, after DNS resolution on the actual
// address being dialed, so a name that resolves to a denied address, including a
// rebinding attack, is blocked rather than trusted.
//
// This guards connections the agent's own Go code makes. Confining a child process
// the agent launches (an inference runtime, say) to no network is a complementary,
// OS-level layer that belongs with the execution sandbox.
package netguard

import (
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// Policy decides which outbound addresses are reachable. The zero Policy denies
// everything (default-deny): a connection is allowed only if its address falls in an
// allowed range, or it is a public address and the policy permits public addresses.
type Policy struct {
	// AllowPublic permits any globally-routable public address while still denying
	// private, loopback, link-local, and the cloud metadata range. This is the
	// anti-SSRF mode for reaching public sources.
	AllowPublic bool
	// Allow is an explicit allowlist of address ranges, permitted even when not
	// public (a specific host's address, or a private range a run is granted).
	Allow []netip.Prefix
}

// DenyAll is the default-deny policy: no outbound connection is permitted.
func DenyAll() Policy { return Policy{} }

// PublicOnly permits public addresses and denies everything private (anti-SSRF).
func PublicOnly() Policy { return Policy{AllowPublic: true} }

// Allows reports whether addr may be connected to under this policy.
func (p Policy) Allows(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap() // compare an IPv4-in-IPv6 address as IPv4
	for _, pre := range p.Allow {
		if pre.Contains(addr) {
			return true
		}
	}
	return p.AllowPublic && IsPublic(addr)
}

// reserved are IANA special-purpose ranges that are not globally routable but are
// not caught by the standard library's address predicates, so a strict public check
// must reject them too. The security-relevant ones include shared CGNAT space, which
// some networks use internally, so reaching it could be a request-forgery path.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes the broadcast address
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 (can map to an internal IPv4)
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// IsPublic reports whether addr is a globally-routable public address. It rejects
// loopback, private (RFC1918 and IPv6 unique-local), link-local (which covers the
// 169.254.169.254 cloud metadata endpoint), multicast, the unspecified address, and
// the IANA special-purpose ranges above, so only an address that can legitimately be
// reached on the public internet is allowed.
func IsPublic(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, pre := range reserved {
		if pre.Contains(addr) {
			return false
		}
	}
	return true
}

// denied is the verdict for a blocked connection.
func denied(host string) error {
	return fault.New(fault.Forbidden, "egress_denied", "netguard: egress to "+host+" denied by policy")
}

// DialControl returns a net.Dialer Control function that enforces p. Because Control
// runs after DNS resolution on the address actually being dialed, it blocks a name
// that resolves to a denied address (a rebinding attack included), not just a
// literal one.
func DialControl(p Policy) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fault.New(fault.Forbidden, "egress_addr", "netguard: cannot parse dial address "+address)
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !p.Allows(addr) {
			return denied(host)
		}
		return nil
	}
}

// Dialer returns a net.Dialer that gates every connection through p, for raw
// socket protocols that are not HTTP (a line-delimited or JSON-RPC service, say).
// It is the raw-socket counterpart of Client: a caller reaching a local,
// operator-supplied service over a loopback port grants exactly that address in p,
// and the policy still blocks anything else, including an address a name rebinds to.
func Dialer(p Policy) *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, Control: DialControl(p)}
}

// Client builds a hardened HTTP client that dials only where p allows, follows a
// bounded number of redirects (re-applying the policy on each hop through the dial
// control) and refuses a non-https redirect, and honors no environment proxy so the
// policy stays authoritative. A request's own scheme is the caller's check; this
// guards where connections may go.
func Client(p Policy) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: DialControl(p)}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fault.New(fault.Terminal, "egress_redirects", "netguard: too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fault.New(fault.Forbidden, "egress_redirect_scheme", "netguard: refusing a non-https redirect to "+req.URL.Host)
			}
			return nil
		},
	}
}
