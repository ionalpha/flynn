package bindguard

import (
	"net"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestCheckHost(t *testing.T) {
	cases := []struct {
		host string
		e    Exposure
		ok   bool
	}{
		// Loopback is always fine, under either exposure.
		{"127.0.0.1", Loopback(), true},
		{"127.0.0.1", Exposed(), true},
		{"::1", Loopback(), true},
		{"127.0.0.5", Loopback(), true}, // all of 127.0.0.0/8 is loopback
		// Wildcard is refused unconditionally, even when exposed.
		{"", Loopback(), false},
		{"", Exposed(), false},
		{"0.0.0.0", Loopback(), false},
		{"0.0.0.0", Exposed(), false},
		{"::", Loopback(), false},
		{"::", Exposed(), false},
		// Non-loopback unicast: refused by default, allowed only when exposed.
		{"10.0.0.5", Loopback(), false},
		{"10.0.0.5", Exposed(), true},
		{"192.168.1.10", Loopback(), false},
		{"192.168.1.10", Exposed(), true},
		{"8.8.8.8", Loopback(), false},
		{"8.8.8.8", Exposed(), true},
		{"fd00::1", Exposed(), true},
		// Multicast is refused even when exposed.
		{"224.0.0.1", Exposed(), false},
		{"ff02::1", Exposed(), false},
		// IPv4-mapped loopback must still read as loopback.
		{"::ffff:127.0.0.1", Loopback(), true},
		{"::ffff:8.8.8.8", Loopback(), false},
		{"::ffff:8.8.8.8", Exposed(), true},
	}
	for _, c := range cases {
		err := CheckHost(c.host, c.e)
		if (err == nil) != c.ok {
			t.Errorf("CheckHost(%q, %+v) err=%v, want ok=%v", c.host, c.e, err, c.ok)
		}
	}
}

func TestCheckHostLocalhostResolvesLoopback(t *testing.T) {
	// "localhost" resolves only to loopback addresses, so it is bindable by default.
	if err := CheckHost("localhost", Loopback()); err != nil {
		t.Errorf("localhost should be loopback-bindable by default: %v", err)
	}
}

func TestListenLoopbackDefault(t *testing.T) {
	ln, err := Listen("tcp", "127.0.0.1:0", Loopback())
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	if host != "127.0.0.1" {
		t.Errorf("bound %s, want loopback", host)
	}
}

func TestListenRefusesWildcardByDefault(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		ln, err := Listen("tcp", addr, Loopback())
		if err == nil {
			_ = ln.Close()
			t.Errorf("Listen(%q) should refuse a wildcard bind by default", addr)
		}
	}
}

func TestListenRefusesWildcardEvenWhenExposed(t *testing.T) {
	// Exposure opts into a named interface, never the wildcard.
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		ln, err := Listen("tcp", addr, Exposed())
		if err == nil {
			_ = ln.Close()
			t.Errorf("Listen(%q) should refuse a wildcard bind even when exposed", addr)
		}
	}
}

func TestListenRefusesNonLoopbackByDefault(t *testing.T) {
	// A non-loopback literal is refused without the opt-in. (No actual bind happens:
	// the check runs before net.Listen, so an unroutable-here address still fails on
	// the policy, not on the OS.)
	ln, err := Listen("tcp", "8.8.8.8:0", Loopback())
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen on a non-loopback address should be refused by default")
	}
	if !strings.Contains(err.Error(), "tunnel") {
		t.Errorf("refusal should point at the tunnel/opt-in path, got %v", err)
	}
}

func TestListenBadAddress(t *testing.T) {
	if _, err := Listen("tcp", "not-an-addr", Loopback()); err == nil {
		t.Fatal("an unparseable listen address should be refused")
	}
}

func TestFreeLoopbackPort(t *testing.T) {
	p, err := FreeLoopbackPort()
	if err != nil {
		t.Fatalf("FreeLoopbackPort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Errorf("port %d out of range", p)
	}
}

// A wildcard or non-loopback host is never bindable by default, for any port: the
// policy is a property of the address, not the port.
func TestCheckHostProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		octet := rapid.IntRange(1, 254)
		// Keep the first octet unicast (avoid the 224-239 multicast and 240+ reserved
		// ranges), so every drawn host is a plain unicast address.
		host := net.IPv4(byte(rapid.IntRange(1, 223).Draw(t, "a")), byte(octet.Draw(t, "b")),
			byte(octet.Draw(t, "c")), byte(octet.Draw(t, "d"))).String()
		isLoopback := strings.HasPrefix(host, "127.")
		// Default (loopback-only): only loopback passes.
		if err := CheckHost(host, Loopback()); (err == nil) != isLoopback {
			t.Fatalf("default CheckHost(%s) err=%v, want loopback-only", host, err)
		}
		// Exposed: any non-multicast unicast passes (these random hosts are unicast).
		if err := CheckHost(host, Exposed()); err != nil {
			t.Fatalf("exposed CheckHost(%s) should pass: %v", host, err)
		}
	})
}
