package integrations

import (
	"testing"

	"pgregory.net/rapid"
)

// TestPropHostConfinement asserts the security invariant of the doer over arbitrary
// hosts and paths: a relative request always resolves to the base host with the base
// path preserved as a prefix, an absolute request to any host that is not the base
// host or in the egress allow-list is always refused, and a host that is in the
// allow-list is always permitted. This is the property that keeps a runtime-authored
// flow from redirecting a credentialed request off-host.
func TestPropHostConfinement(t *testing.T) {
	hostGen := rapid.StringMatching(`[a-z][a-z0-9]{2,9}\.example\.(com|net|org)`)

	rapid.Check(t, func(rt *rapid.T) {
		baseHost := hostGen.Draw(rt, "baseHost")
		allowed := rapid.SliceOfN(hostGen, 0, 3).Draw(rt, "allowed")
		// The doer needs no auth provider or transport to resolve a URL.
		d := newTransportDoer(nil, nil, nil, "https://"+baseHost+"/v1", allowed)

		// A relative URL resolves to the base host, keeping the base path prefix.
		rel := "/" + rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, "path")
		u, err := d.resolveURL(rel)
		if err != nil {
			rt.Fatalf("relative url refused: %v", err)
		}
		if u.Hostname() != baseHost {
			rt.Fatalf("relative url escaped base host: %s", u.Hostname())
		}
		if got := u.EscapedPath(); len(got) < 3 || got[:3] != "/v1" {
			rt.Fatalf("base path not preserved: %q", got)
		}

		// An absolute URL is permitted exactly when its host is allowed.
		target := hostGen.Draw(rt, "targetHost")
		_, err = d.resolveURL("https://" + target + "/x")
		permitted := err == nil
		shouldPermit := target == baseHost
		for _, a := range allowed {
			if a == target {
				shouldPermit = true
			}
		}
		if permitted != shouldPermit {
			rt.Fatalf("host %q: permitted=%v want %v (base=%q allow=%v)", target, permitted, shouldPermit, baseHost, allowed)
		}
	})
}
