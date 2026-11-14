package bindguard

import (
	"net/netip"
	"testing"

	"pgregory.net/rapid"
)

// TestFailClosedExposure proves the zero Exposure binds loopback only, and that no
// exposure setting permits a wildcard, multicast, empty or invalid host.
func TestFailClosedExposure(t *testing.T) {
	t.Run("the zero exposure is loopback-only", func(t *testing.T) {
		if (Exposure{}) != Loopback() {
			t.Fatal("the zero Exposure is not Loopback()")
		}
		for _, h := range []string{"192.168.1.5", "10.0.0.1", "8.8.8.8", "fe80::1", "2001:4860:4860::8888", "::ffff:10.0.0.1"} {
			if err := CheckHost(h, Exposure{}); err == nil {
				t.Errorf("the zero exposure bound %s", h)
			}
		}
		for _, h := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
			if err := CheckHost(h, Exposure{}); err != nil {
				t.Errorf("loopback %s refused: %v", h, err)
			}
		}
	})
	t.Run("no exposure permits a wildcard, empty or malformed host", func(t *testing.T) {
		for _, e := range []Exposure{{}, Loopback(), Exposed()} {
			for _, h := range []string{"", "0.0.0.0", "::", "::ffff:0.0.0.0", "224.0.0.1", "ff02::1", "239.1.1.1", "127.0.0.1:80", "not a host", " "} {
				if err := CheckHost(h, e); err == nil {
					t.Errorf("exposure %+v bound %q", e, h)
				}
			}
		}
	})
	t.Run("an invalid parsed address is refused", func(t *testing.T) {
		for _, e := range []Exposure{{}, Exposed()} {
			if err := checkAddr(netip.Addr{}, e); err == nil {
				t.Errorf("exposure %+v accepted the zero address", e)
			}
		}
	})
	t.Run("a non-loopback address needs the opt-in", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			var b [4]byte
			for i := range b {
				b[i] = rapid.Byte().Draw(rt, "byte")
			}
			a := netip.AddrFrom4(b)
			if err := checkAddr(a, Exposure{}); err == nil && !a.IsLoopback() {
				rt.Fatalf("the zero exposure bound %s", a)
			}
		})
	})
	t.Run("Listen refuses an unparseable address without opening a socket", func(t *testing.T) {
		for _, a := range []string{"", "8080", ":8080", "0.0.0.0:0", "[::]:0"} {
			if ln, err := Listen("tcp", a, Exposed()); err == nil {
				_ = ln.Close()
				t.Errorf("Listen bound %q", a)
			}
		}
	})
}
