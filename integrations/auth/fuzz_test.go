package auth_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/integrations/auth"
)

// FuzzApply throws hostile bytes at every field an integration spec and a vault
// value control: the scheme selector, the api_key parameter name, the value
// prefix, and the resolved credential. The invariants hold for every input: Apply
// never panics, and any request it accepts (returns nil for) carries only
// well-formed headers, so a malformed credential or config can never inject a
// second header.
func FuzzApply(f *testing.F) {
	seeds := []struct {
		scheme        byte
		param, prefix string
		token         string
	}{
		{0, "", "", "tok"},
		{1, "", "", "user:pass"},
		{2, "", "", "bearer-token"},
		{3, "X-API-Key", "", "key"},
		{3, "Authorization", "Token ", "key"},
		{3, "api_key", "", "key"},
		{2, "", "", "good\r\nX-Injected: evil"},
		{3, "X-Key\r\nEvil: 1", "", "key"},
		{3, "X-Key", "pre\r\nfix", "key"},
	}
	for _, s := range seeds {
		f.Add(s.scheme, s.param, s.prefix, s.token)
	}

	f.Fuzz(func(t *testing.T, schemeSel byte, param, prefix, token string) {
		cfg := configFor(schemeSel, param, prefix)
		p, err := auth.FromConfig(cfg)
		if err != nil {
			return // a rejected config never reaches Apply
		}
		req, err := http.NewRequest(http.MethodGet, "https://api.example.com/p?a=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		// Provide the credential under every reference the config might use.
		src := mapSource{"TOKEN": token, "USER": token, "PASS": token}
		if err := p.Apply(context.Background(), req, src); err != nil {
			return // refused: nothing written
		}
		for name, vals := range req.Header {
			if strings.ContainsAny(name, "\r\n \t") {
				t.Fatalf("accepted unsafe header name %q", name)
			}
			for _, v := range vals {
				if strings.ContainsAny(v, "\r\n") {
					t.Fatalf("accepted header %q with injected value %q", name, v)
				}
			}
		}
	})
}

// configFor maps a fuzzed selector byte to one of the auth schemes, wiring in the
// fuzzed parameter name and prefix where the scheme uses them.
func configFor(sel byte, param, prefix string) auth.Config {
	switch sel % 4 {
	case 0:
		return auth.Config{Type: auth.SchemeNone}
	case 1:
		return auth.Config{Type: auth.SchemeBasic, UsernameRef: "USER", PasswordRef: "PASS"}
	case 2:
		return auth.Config{Type: auth.SchemeBearer, TokenRef: "TOKEN"}
	default:
		in := auth.InHeader
		if len(prefix)%2 == 0 {
			in = auth.InQuery
		}
		return auth.Config{Type: auth.SchemeAPIKey, TokenRef: "TOKEN", Param: param, Prefix: prefix, In: in}
	}
}
