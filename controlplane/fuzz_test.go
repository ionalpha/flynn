package controlplane

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
)

// FuzzVerifyToken drives capability-token verification with a fully
// attacker-controlled token string against a fixed trusted issuer key. A bearer
// presents this string, so the bar is that no input panics (the chain runs base64
// decode, JSON decode, and ed25519 signature checks over attacker bytes, including
// a would-be signer key of any length) and that a token which verifies always fails
// closed to a real subject rather than an empty Authority.
func FuzzVerifyToken(f *testing.F) {
	issuer, err := GenerateIdentity()
	if err != nil {
		f.Fatal(err)
	}
	holder, err := GenerateIdentity()
	if err != nil {
		f.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// A genuinely valid root token and a one-hop attenuation, so the fuzzer starts
	// from the real chain structure and mutates outward from it.
	root, err := Issue(issuer, holder.Public(), ScopeOperator, capability.NewGrant("a", "b"), now.Add(time.Hour))
	if err != nil {
		f.Fatal(err)
	}
	att, err := Attenuate(root, holder, holder.Public(), ScopeRead, []string{"a"}, now.Add(time.Hour))
	if err != nil {
		f.Fatal(err)
	}

	seeds := []string{
		root,
		att,
		"",
		"not base64 %%%",
		base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"blocks":[]}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"blocks":[{"payload":"e30","sig":"AA"}]}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"blocks":[{"payload":null,"sig":null}]}`)),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	issuerPub := issuer.Public()
	f.Fuzz(func(t *testing.T, tok string) {
		auth, err := Verify(tok, issuerPub, now)
		if err != nil {
			return // failing closed on a malformed or forged token is the correct outcome
		}
		// A token that verifies must name a subject: the chain guarantees the leaf
		// audience is a full-length key, so an accepted-but-empty Authority would be
		// an authorization bypass.
		if auth.Subject == "" {
			t.Fatalf("Verify accepted a token but returned an empty Authority subject")
		}
	})
}

// FuzzParseApprovals drives approval-header parsing with fully attacker-controlled
// header values. Every X-Flynn-Approval value runs base64 decode then JSON decode over
// bytes a caller chose, so the bar is that no value panics and that parsing stays
// bounded by the headers presented: a single header can never fan out into extra
// approvals in the quorum the verifier then tallies. Values that do not decode are
// skipped by design, and an approval that decodes still has to carry a signature the
// verifier checks, so this target guards the parse, not the authorization decision.
func FuzzParseApprovals(f *testing.F) {
	enc := base64.StdEncoding.EncodeToString
	seeds := []string{
		"",
		"not base64 %%%",
		enc([]byte(`{}`)),
		enc([]byte(`{"keyId":"k1","signature":"AAAA"}`)),
		enc([]byte(`{"envelope":{},"keyId":"k1","signature":null}`)),
		enc([]byte(`{"envelope":null}`)),
		enc([]byte(`{"keyId":123}`)),
		enc([]byte(`[]`)),
		enc([]byte(`null`)),
		enc(make([]byte, 512)),
	}
	for _, s := range seeds {
		f.Add(s, "")
	}

	f.Fuzz(func(t *testing.T, first, second string) {
		r, err := http.NewRequest(http.MethodPost, "/v1/Widget/w1/restart", nil)
		if err != nil {
			t.Skip() // a header value the request builder rejects never reaches a handler
		}
		r.Header.Add(approvalHeader, first)
		r.Header.Add(approvalHeader, second)

		got := parseApprovals(r)
		if len(got) > 2 {
			t.Fatalf("parseApprovals returned %d approvals for 2 headers", len(got))
		}
		if again := parseApprovals(r); len(again) != len(got) {
			t.Fatalf("parseApprovals is not deterministic: %d then %d", len(got), len(again))
		}
	})
}
