package netguard_test

import (
	"net/netip"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/netguard"
)

// TestFailClosedPolicy proves the zero Policy and every address it cannot positively
// place are refused. An empty AllowHosts list imposes no name restriction by design (the
// address rules still decide); that is pinned so changing it is a decision.
func TestFailClosedPolicy(t *testing.T) {
	t.Run("the zero policy denies every address", func(t *testing.T) {
		var zero netguard.Policy
		rapid.Check(t, func(rt *rapid.T) {
			var b [16]byte
			for i := range b {
				b[i] = rapid.Byte().Draw(rt, "byte")
			}
			addr := netip.AddrFrom16(b)
			if zero.Allows(addr) || zero.Allows(addr.Unmap()) {
				rt.Fatalf("the zero policy allowed %s", addr)
			}
		})
	})
	t.Run("DenyAll equals the zero policy", func(t *testing.T) {
		if netguard.DenyAll().AllowPublic || len(netguard.DenyAll().Allow) != 0 {
			t.Fatal("DenyAll grants something")
		}
	})
	t.Run("an invalid address is denied by every policy", func(t *testing.T) {
		wide := netguard.Policy{AllowPublic: true, Allow: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}
		for _, p := range []netguard.Policy{{}, netguard.PublicOnly(), wide} {
			if p.Allows(netip.Addr{}) {
				t.Errorf("the zero address was allowed by %+v", p)
			}
		}
		if netguard.IsPublic(netip.Addr{}) {
			t.Error("the zero address is public")
		}
	})
	t.Run("a zero prefix in the allowlist allows nothing", func(t *testing.T) {
		p := netguard.Policy{Allow: []netip.Prefix{{}}}
		for _, s := range []string{"8.8.8.8", "10.0.0.1", "127.0.0.1", "::1"} {
			if p.Allows(netip.MustParseAddr(s)) {
				t.Errorf("a zero prefix allowed %s", s)
			}
		}
	})
	t.Run("public-only still refuses non-routable ranges", func(t *testing.T) {
		p := netguard.PublicOnly()
		for _, s := range []string{
			"0.0.0.0", "::", "127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "169.254.169.254",
			"100.64.0.1", "240.0.0.1", "255.255.255.255", "224.0.0.1", "fe80::1", "fc00::1",
			"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1",
		} {
			if p.Allows(netip.MustParseAddr(s)) {
				t.Errorf("public-only allowed %s", s)
			}
		}
	})
	t.Run("a name outside a non-empty allowlist is denied", func(t *testing.T) {
		p := netguard.Policy{AllowHosts: []string{"api.example.com", ".openai.com"}}
		for _, h := range []string{"", "example.com", "evil-openai.com", "openai.com.evil.test", "api.example.com.evil.test", "xapi.example.com"} {
			if p.AllowsHost(h) {
				t.Errorf("the name %q passed the allowlist", h)
			}
		}
	})
	t.Run("an empty allowlist imposes no name restriction by design", func(t *testing.T) {
		if !(netguard.Policy{}).AllowsHost("anything.test") {
			t.Fatal("an empty allowlist must leave the address rules to decide")
		}
	})
}
