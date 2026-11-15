// Package netguard is the default-deny egress policy for connections the agent's
// own Go code makes, and a forward [Proxy] that applies it to a child process.
// The address check runs at connect time on the resolved address, so a name
// that resolves or rebinds to a private or cloud-metadata address is blocked.
// Confining a child to no network at all is the sandbox package's job.
package netguard

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// Policy decides which outbound addresses are reachable. The zero Policy denies
// everything: an address must fall in Allow, or be public with AllowPublic set.
//
//flynn:policy TestFailClosedPolicy
type Policy struct {
	// AllowPublic permits globally routable addresses and still denies private,
	// loopback, link-local and cloud metadata ranges.
	AllowPublic bool
	// Allow lists ranges permitted even when not public.
	Allow []netip.Prefix
	// AllowHosts, when non-empty, restricts destinations by requested name
	// (see [Policy.AllowsHost]). Only the [Proxy] sees the name, so only it
	// applies this. A destination must pass both this and the address rules, so
	// an allowed name that resolves to a private address is still denied.
	AllowHosts []string
}

// DenyAll is the default-deny policy: no outbound connection is permitted.
func DenyAll() Policy { return Policy{} }

// PublicOnly permits public addresses only, which blocks request forgery to
// internal hosts.
func PublicOnly() Policy { return Policy{AllowPublic: true} }

// Allows reports whether addr may be connected to under this policy.
func (p Policy) Allows(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap() // so ::ffff:10.0.0.1 matches 10.0.0.0/8
	for _, pre := range p.Allow {
		if pre.Contains(addr) {
			return true
		}
	}
	return p.AllowPublic && IsPublic(addr)
}

// AllowsHost reports whether host, without a port, passes AllowHosts. An empty
// list permits any name. Matching ignores case and a trailing dot; a dotted
// entry (".example.com") matches the name and every subdomain. It never widens
// what [Policy.Allows] denies.
func (p Policy) AllowsHost(host string) bool {
	if len(p.AllowHosts) == 0 {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range p.AllowHosts {
		a := strings.ToLower(entry)
		if strings.HasPrefix(a, ".") {
			if h == a[1:] || strings.HasSuffix(h, a) {
				return true
			}
			continue
		}
		if h == a {
			return true
		}
	}
	return false
}

// reserved are IANA special-purpose ranges the netip predicates miss. CGNAT
// space is used internally on some networks, so reaching it can be request
// forgery.
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

// IsPublic reports whether addr is globally routable. It rejects loopback,
// RFC1918 and IPv6 unique-local, link-local (including the 169.254.169.254
// metadata endpoint), multicast, unspecified and the IANA reserved ranges.
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

func denied(host string) error {
	return fault.New(fault.Forbidden, "egress_denied", "netguard: egress to "+host+" denied by policy")
}

// Decision is one dial-time egress verdict. Host is the resolved address
// literal.
type Decision struct {
	Host    string
	Allowed bool
	Reason  string
}

// Observer receives each dial's [Decision]. It travels on the context, so a run
// records its egress without netguard importing the recorder. It runs on the
// dial goroutine and must not block.
type Observer func(Decision)

type observerKey struct{}

// WithObserver returns a context whose dials report to obs. A nil obs returns
// ctx unchanged.
func WithObserver(ctx context.Context, obs Observer) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, observerKey{}, obs)
}

func observerFromContext(ctx context.Context) Observer {
	obs, _ := ctx.Value(observerKey{}).(Observer)
	return obs
}

// verdict must match [Policy.Allows] branch for branch, so the recorded reason
// never disagrees with what was enforced.
func verdict(p Policy, host string) (allowed bool, reason string) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false, "unresolved address"
	}
	addr = addr.Unmap()
	for _, pre := range p.Allow {
		if pre.Contains(addr) {
			return true, "allowlisted"
		}
	}
	if p.AllowPublic && IsPublic(addr) {
		return true, "public"
	}
	if !IsPublic(addr) {
		return false, "private or reserved address"
	}
	return false, "public egress not permitted"
}

// DialControl returns a net.Dialer Control function that enforces p. Control
// runs after DNS resolution, so it also blocks a name that rebinds to a denied
// address.
func DialControl(p Policy) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fault.New(fault.Forbidden, "egress_addr", "netguard: cannot parse dial address "+address)
		}
		if allowed, _ := verdict(p, host); !allowed {
			return denied(host)
		}
		return nil
	}
}

// DialControlContext is [DialControl] that also reports to the context's
// [Observer]. [Dialer] and [Client] install it.
func DialControlContext(p Policy) func(ctx context.Context, network, address string, c syscall.RawConn) error {
	return func(ctx context.Context, _, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fault.New(fault.Forbidden, "egress_addr", "netguard: cannot parse dial address "+address)
		}
		allowed, reason := verdict(p, host)
		if obs := observerFromContext(ctx); obs != nil {
			obs(Decision{Host: host, Allowed: allowed, Reason: reason})
		}
		if !allowed {
			return denied(host)
		}
		return nil
	}
}

// Dialer returns a net.Dialer that enforces p, for protocols other than HTTP.
// To reach a local service, grant exactly its loopback address in p.
func Dialer(p Policy) *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, ControlContext: DialControlContext(p)}
}

// Client returns an HTTP client that dials only where p allows. It follows at
// most 5 redirects, each re-checked at dial, refuses a non-https redirect, and
// ignores proxy environment variables, which would bypass p. The scheme of the
// first request is the caller's check.
func Client(p Policy) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, ControlContext: DialControlContext(p)}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			// Go's default keeps 2 idle connections per host and never reaps
			// them. Traffic is bursty against a few hosts, so keep 8 per host to
			// avoid re-handshakes and reap idle ones after 90s.
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
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
