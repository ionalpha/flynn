package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"pgregory.net/rapid"
)

func addr(s string) netip.Addr { a, _ := netip.ParseAddr(s); return a }

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":          true,  // public
		"1.1.1.1":          true,  // public
		"127.0.0.1":        false, // loopback
		"10.0.0.5":         false, // private
		"192.168.1.10":     false, // private
		"172.16.0.1":       false, // private
		"169.254.169.254":  false, // link-local: cloud metadata endpoint
		"224.0.0.1":        false, // multicast
		"0.0.0.0":          false, // unspecified
		"::1":              false, // IPv6 loopback
		"fd00::1":          false, // IPv6 unique-local
		"fe80::1":          false, // IPv6 link-local
		"2606:4700::1111":  true,  // public IPv6
		"::ffff:127.0.0.1": false, // IPv4-mapped loopback must still be loopback
		"::ffff:8.8.8.8":   true,  // IPv4-mapped public
		"0.0.0.1":          false, // 0.0.0.0/8 "this network", reserved
		"100.64.0.1":       false, // shared address space (CGNAT)
		"198.18.0.1":       false, // benchmarking
		"240.0.0.1":        false, // reserved
		"2001:db8::1":      false, // documentation
	}
	for s, want := range cases {
		if got := IsPublic(addr(s)); got != want {
			t.Errorf("IsPublic(%s)=%v, want %v", s, got, want)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("the zero address is not public")
	}
}

func TestPolicyAllows(t *testing.T) {
	// Default-deny: the zero policy permits nothing.
	if DenyAll().Allows(addr("8.8.8.8")) || DenyAll().Allows(addr("127.0.0.1")) {
		t.Fatal("DenyAll must permit nothing")
	}
	// Public-only: public yes, private/loopback/metadata no.
	pub := PublicOnly()
	if !pub.Allows(addr("8.8.8.8")) {
		t.Fatal("PublicOnly must permit a public address")
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1"} {
		if pub.Allows(addr(s)) {
			t.Fatalf("PublicOnly must deny %s", s)
		}
	}
	// Allowlist: a granted private range is permitted even without AllowPublic.
	p := Policy{Allow: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}}
	if !p.Allows(addr("10.1.2.3")) {
		t.Fatal("an allowlisted range must be permitted")
	}
	if p.Allows(addr("10.2.0.1")) || p.Allows(addr("8.8.8.8")) {
		t.Fatal("an allowlist must not permit addresses outside it")
	}
}

// TestClientEnforcesPolicy is the integration check: a DenyAll client reaches
// nothing, and a PublicOnly client refuses a loopback server (which a test server
// always is), so neither the agent's own request nor a redirect can be steered to a
// blocked address.
func TestClientEnforcesPolicy(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("blocked"))
	}))
	t.Cleanup(srv.Close)

	for _, p := range []Policy{DenyAll(), PublicOnly()} {
		c := Client(p)
		// Trust the test cert so any failure is the dial guard, not TLS.
		c.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("policy %+v must block the loopback server", p)
		}
	}
}

// TestDialControlBlocksAndAllows checks the dial guard directly: it permits a public
// address and rejects loopback, the metadata endpoint, and an unparseable address.
func TestDialControlBlocksAndAllows(t *testing.T) {
	ctrl := DialControl(PublicOnly())
	if err := ctrl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("a public address should be allowed: %v", err)
	}
	for _, a := range []string{"127.0.0.1:443", "169.254.169.254:80", "10.0.0.1:1", "garbage"} {
		if err := ctrl("tcp", a, nil); err == nil {
			t.Fatalf("address %q should be blocked", a)
		}
	}
}

// FuzzDialControl throws arbitrary dial addresses at the guard, which sees the
// address the client is about to connect to (after DNS resolution). Invariants: it
// never panics, and any address it allows is a public IP under the policy, so
// localhost, a private network, link-local, or the metadata endpoint is never dialed.
func FuzzDialControl(f *testing.F) {
	for _, s := range []string{
		"1.2.3.4:443", "127.0.0.1:80", "[::1]:443", "169.254.169.254:80", "10.0.0.1:1",
		"8.8.8.8:53", "[fd00::1]:443", "garbage", "host:port", ":443", "", "1.2.3.4",
		"[::ffff:127.0.0.1]:443",
	} {
		f.Add(s)
	}
	ctrl := DialControl(PublicOnly())
	f.Fuzz(func(t *testing.T, address string) {
		if err := ctrl("tcp", address, nil); err == nil {
			// Verify with the same parsing the guard uses, then re-check publicness.
			host, _, splitErr := net.SplitHostPort(address)
			a, parseErr := netip.ParseAddr(host)
			if splitErr != nil || parseErr != nil || !IsPublic(a) {
				t.Fatalf("DialControl allowed a non-public address: %q", address)
			}
		}
	})
}

// TestAllowsProperty checks the policy contract over random addresses: PublicOnly
// allows an address exactly when it is public, and DenyAll allows nothing.
func TestAllowsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var b [16]byte
		for i := range b {
			b[i] = byte(rapid.IntRange(0, 255).Draw(rt, "b"))
		}
		var a netip.Addr
		if rapid.Bool().Draw(rt, "v4") {
			a = netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
		} else {
			a = netip.AddrFrom16(b)
		}
		if PublicOnly().Allows(a) != IsPublic(a) {
			rt.Fatalf("PublicOnly disagrees with IsPublic for %s", a)
		}
		if DenyAll().Allows(a) {
			rt.Fatalf("DenyAll allowed %s", a)
		}
	})
}
