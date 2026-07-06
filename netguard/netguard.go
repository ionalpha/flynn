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
	// AllowHosts, when non-empty, restricts egress to these destination host names,
	// checked on the name the client asked to reach before it is resolved. An entry is a
	// host name matched case-insensitively; an entry beginning with a dot (".openai.com")
	// also matches any subdomain of it. It composes with the address rules above rather
	// than replacing them: a destination must pass both the name gate here and the
	// resolved-address gate (AllowPublic/Allow), so an allowlisted name that resolves to a
	// private or rebinding address is still denied. It is enforced where the destination
	// name is known, the egress proxy a child process is pointed at, so it governs a
	// process whose own code we do not control: "deny all egress except these providers".
	// An empty AllowHosts imposes no name restriction (the address rules still apply).
	AllowHosts []string
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

// AllowsHost reports whether host (a destination name without a port) passes the name
// allowlist. An empty allowlist permits any name, so the address rules alone decide;
// otherwise the name must equal an entry, or be a subdomain of a dotted entry
// (".example.com" matches example.com and any subdomain), compared case-insensitively. A
// trailing dot on a fully-qualified name is ignored. It is a name gate only: a caller
// still applies the address rules (Allows) on the resolved address, so this never widens
// what an address rule denies.
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

// Decision is one egress verdict netguard reached at dial time: the host it was asked
// to reach (the literal address resolved before connecting), whether the policy allowed
// it, and a short reason for the verdict. It is what an Observer is handed, so a run can
// record its own outbound network decisions without netguard depending on the recording
// layer.
type Decision struct {
	Host    string
	Allowed bool
	Reason  string
}

// Observer is notified of each egress decision netguard makes on a dial. It is
// registered on a context (see WithObserver), so netguard reports a run's outbound
// verdicts without importing the recording layer, and a dial made outside any run
// reports to no one. It runs on the dial goroutine, so an implementation must not block.
type Observer func(Decision)

// observerKey is the private context key the egress observer is carried under.
type observerKey struct{}

// WithObserver returns a context that reports every netguard egress decision made on a
// dial using it to obs. A nil observer returns ctx unchanged, so a caller can seed
// unconditionally.
func WithObserver(ctx context.Context, obs Observer) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, observerKey{}, obs)
}

// observerFromContext returns the observer seeded on ctx, or nil when none is.
func observerFromContext(ctx context.Context) Observer {
	obs, _ := ctx.Value(observerKey{}).(Observer)
	return obs
}

// verdict decides whether host may be dialed under p and gives a short honest reason
// for the decision, the label the egress record and panel show. It mirrors Allows
// exactly (allowlist first, then public), so the reported verdict can never disagree
// with the enforcement below it; the reason only names which branch decided.
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
		if allowed, _ := verdict(p, host); !allowed {
			return denied(host)
		}
		return nil
	}
}

// DialControlContext is the context-aware DialControl: it enforces p identically and,
// when an Observer is seeded on ctx, reports the decision to it before returning. It is
// the hook Dialer and Client install, so a run that seeds an observer records every dial
// its own code makes; a dial with no observer on its context reports nothing and behaves
// exactly as DialControl.
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

// Dialer returns a net.Dialer that gates every connection through p, for raw
// socket protocols that are not HTTP (a line-delimited or JSON-RPC service, say).
// It is the raw-socket counterpart of Client: a caller reaching a local,
// operator-supplied service over a loopback port grants exactly that address in p,
// and the policy still blocks anything else, including an address a name rebinds to.
func Dialer(p Policy) *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, ControlContext: DialControlContext(p)}
}

// Client builds a hardened HTTP client that dials only where p allows, follows a
// bounded number of redirects (re-applying the policy on each hop through the dial
// control) and refuses a non-https redirect, and honors no environment proxy so the
// policy stays authoritative. A request's own scheme is the caller's check; this
// guards where connections may go.
func Client(p Policy) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, ControlContext: DialControlContext(p)}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			// Pool tuning: without these, concurrent calls to one API host keep
			// only Go's default 2 idle connections and re-handshake the rest, and
			// idle connections are never reaped. Agent tool traffic is bursty
			// against a few hosts (one LLM endpoint, a handful of integrations), so
			// keep enough idle connections per host to avoid re-handshaking and cap
			// the total, reaping idle ones after 90s.
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
