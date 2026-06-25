package auth_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/integrations/auth"
)

// drawBytes draws an arbitrary byte string, including control bytes and high
// bytes, so the generated credential covers the hostile inputs a real vault value
// or a malformed config might carry.
func drawBytes(rt *rapid.T, label string) string {
	return string(rapid.SliceOf(rapid.Byte()).Draw(rt, label))
}

// headerSafe reports whether every header name and value on req is well formed: no
// control bytes that could have injected a second header. It is the invariant Apply
// must preserve for every input it accepts without error.
func headerSafe(req *http.Request) bool {
	for name, vals := range req.Header {
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(name, " \t") {
			return false
		}
		for _, v := range vals {
			if strings.ContainsAny(v, "\r\n") {
				return false
			}
		}
	}
	return true
}

// Property: whatever bytes a token or prefix carries, Apply either refuses with an
// error or produces a request with only well-formed headers. It never writes a
// header that injects another. This is the core safety guarantee across bearer and
// header-placed api_key, the two schemes that write an attacker-influenced value.
func TestProp_ApplyNeverInjectsHeader(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		token := drawBytes(rt, "token")
		scheme := rapid.SampledFrom([]auth.Scheme{auth.SchemeBearer, auth.SchemeAPIKey}).Draw(rt, "scheme")

		var cfg auth.Config
		switch scheme {
		case auth.SchemeBearer:
			cfg = auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"}
		default:
			cfg = auth.Config{
				Type:     auth.SchemeAPIKey,
				TokenRef: "TOKEN",
				Param:    "X-API-Key",
				Prefix:   drawBytes(rt, "prefix"),
			}
		}
		p, err := auth.FromConfig(cfg)
		if err != nil {
			return // a rejected config never gets to Apply
		}
		req := newReq(t)
		if err := p.Apply(context.Background(), req, mapSource{"TOKEN": token}); err != nil {
			return // refused: nothing was written
		}
		if !headerSafe(req) {
			rt.Fatalf("Apply produced an unsafe header from token %q", token)
		}
	})
}

// Property: a query-placed api_key round-trips through the URL for any token bytes,
// without error and without breaking the URL, because url.Values percent-encodes
// it. The existing query parameters survive unchanged.
func TestProp_APIKeyQueryRoundTrips(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		token := drawBytes(rt, "token")
		p, err := auth.FromConfig(auth.Config{
			Type: auth.SchemeAPIKey, TokenRef: "TOKEN", Param: "api_key", In: auth.InQuery,
		})
		if err != nil {
			rt.Fatalf("FromConfig: %v", err)
		}
		req := newReq(t)
		if err := p.Apply(context.Background(), req, mapSource{"TOKEN": token}); err != nil {
			rt.Fatalf("query placement should never error, got %v", err)
		}
		q := req.URL.Query()
		if got := q.Get("api_key"); got != token {
			rt.Fatalf("api_key round-trip: got %q, want %q", got, token)
		}
		if got := q.Get("page"); got != "2" {
			rt.Fatalf("existing query param clobbered: page = %q", got)
		}
	})
}

// Property: Apply is idempotent for header schemes. Applying the same provider
// twice leaves exactly one header with one value, because each provider sets rather
// than appends.
func TestProp_ApplyIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Restrict to a header-safe token so both applies succeed and we can compare.
		token := rapid.StringMatching(`[a-zA-Z0-9._-]{1,40}`).Draw(rt, "token")
		p, err := auth.FromConfig(auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"})
		if err != nil {
			rt.Fatalf("FromConfig: %v", err)
		}
		src := mapSource{"TOKEN": token}
		req := newReq(t)
		if err := p.Apply(context.Background(), req, src); err != nil {
			rt.Fatalf("first apply: %v", err)
		}
		if err := p.Apply(context.Background(), req, src); err != nil {
			rt.Fatalf("second apply: %v", err)
		}
		if got := req.Header.Values("Authorization"); len(got) != 1 {
			rt.Fatalf("Authorization has %d values, want 1: %v", len(got), got)
		}
		if got, want := req.Header.Get("Authorization"), "Bearer "+token; got != want {
			rt.Fatalf("Authorization = %q, want %q", got, want)
		}
	})
}
