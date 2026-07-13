package controlplane

import (
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
)

// mustIdentity mints an identity for a test.
func mustIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

// TestIssueRefusesAnIncompleteRequest checks a token is never minted without an issuer to
// sign it or an audience to bind it to. Either omission would produce a credential nobody
// could verify or anybody could present.
func TestIssueRefusesAnIncompleteRequest(t *testing.T) {
	id := mustIdentity(t)
	future := time.Unix(2_000_000_000, 0)

	if _, err := Issue(nil, id.Public(), ScopeRead, capability.AllowAll(), future); err == nil {
		t.Error("Issue with no issuer must fail")
	}
	if _, err := Issue(id, ed25519.PublicKey("short"), ScopeRead, capability.AllowAll(), future); err == nil {
		t.Error("Issue with an undersized audience key must fail")
	}
}

// TestAttenuateRefusesWhatItCannotNarrow checks every way an attenuation is refused: no
// holder to sign with, no audience to delegate to, a token that is not a token, an empty
// chain, and a holder that is not the chain's current audience. The last is the one that
// matters most: only the current holder may delegate onward.
func TestAttenuateRefusesWhatItCannotNarrow(t *testing.T) {
	issuer, holder, stranger := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	future := time.Unix(2_000_000_000, 0)
	tok, err := Issue(issuer, holder.Public(), ScopeOperator, capability.NewGrant("a", "b"), future)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	empty, err := encodeToken(capToken{})
	if err != nil {
		t.Fatalf("encode empty token: %v", err)
	}
	// A chain whose leaf block is signed but not decodable, so parsing the leaf fails
	// after the token itself decodes.
	sealed, err := sign(capBlock{}, holder.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sealed.Payload = []byte("not a block")
	garbled, err := encodeToken(capToken{Blocks: []sealedBlock{sealed}})
	if err != nil {
		t.Fatalf("encode garbled token: %v", err)
	}

	cases := []struct {
		name     string
		tok      string
		holder   *Identity
		audience ed25519.PublicKey
		want     string
	}{
		{"no holder", tok, nil, stranger.Public(), "nil holder"},
		{"undersized audience", tok, holder, ed25519.PublicKey("short"), "invalid audience key"},
		{"not a token", "!!!not base64!!!", holder, stranger.Public(), "decode token"},
		{"empty chain", empty, holder, stranger.Public(), "empty token"},
		{"undecodable leaf block", garbled, holder, stranger.Public(), "malformed block"},
		{"holder is not the audience", tok, stranger, stranger.Public(), "not the token's current audience"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Attenuate(tc.tok, tc.holder, tc.audience, ScopeRead, []string{"a"}, future)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Attenuate error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestVerifyRefusesAChainItCannotCheck checks verification fails closed on every input it
// cannot make sense of: an issuer key of the wrong size, a block whose payload is not a
// block, and a block naming an audience that is not a key. None of these may resolve to a
// usable authority.
func TestVerifyRefusesAChainItCannotCheck(t *testing.T) {
	issuer, holder := mustIdentity(t), mustIdentity(t)
	future := time.Unix(2_000_000_000, 0)
	now := time.Unix(1_700_000_000, 0)

	tok, err := Issue(issuer, holder.Public(), ScopeOperator, capability.AllowAll(), future)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := Verify(tok, ed25519.PublicKey("short"), now); err == nil {
		t.Error("an issuer key of the wrong size must verify nothing")
	}

	// A block correctly signed by the issuer whose payload is not a block at all.
	junk := sealedBlock{Payload: []byte("not a block"), Sig: ed25519.Sign(issuer.priv, []byte("not a block"))}
	junkTok, err := encodeToken(capToken{Blocks: []sealedBlock{junk}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := Verify(junkTok, issuer.Public(), now); err == nil ||
		!strings.Contains(err.Error(), "malformed block") {
		t.Errorf("Verify error = %v, want a malformed-block error", err)
	}

	// A well-formed block naming an audience that is not an Ed25519 key. The chain would
	// otherwise resolve to a principal id nobody could ever prove possession of.
	blk := capBlock{Scope: ScopeRead, AllowAll: true, Audience: []byte("short"), NotAfter: future.Unix()}
	sb, err := sign(blk, issuer.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	badAud, err := encodeToken(capToken{Blocks: []sealedBlock{sb}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := Verify(badAud, issuer.Public(), now); err == nil ||
		!strings.Contains(err.Error(), "malformed audience") {
		t.Errorf("Verify error = %v, want a malformed-audience error", err)
	}
}

// TestSignProofRefusesAnUnusableRequest checks a proof is never produced without a key to
// sign it or a nonce to make it single-use. A proof with a reused nonce is a replayable
// one, so an empty nonce is refused at the source rather than at the server.
func TestSignProofRefusesAnUnusableRequest(t *testing.T) {
	if _, err := SignProof(nil, http.MethodGet, "/v1/Widget", "n1", time.Unix(1, 0)); err == nil {
		t.Error("SignProof with no holder must fail")
	}
	if _, err := SignProof(mustIdentity(t), http.MethodGet, "/v1/Widget", "", time.Unix(1, 0)); err == nil {
		t.Error("SignProof with an empty nonce must fail")
	}
}

// TestPossessionRefusesAPrincipalThatIsNotAKey checks the possession layer fails closed
// when it is mounted over an authenticator whose principals are not key-identified: there
// is no key to demand possession of, so the request is refused rather than admitted
// unproven.
func TestPossessionRefusesAPrincipalThatIsNotAKey(t *testing.T) {
	inner := fixedAuth{prin: Principal{ID: "operator-token", Scope: ScopeOperator}}
	p := RequirePossession(inner, nil) // a nil clock falls back to the system clock

	if _, err := p.Authenticate(proofReq(t, http.MethodGet, "/v1/Widget", "anything")); err == nil {
		t.Fatal("a principal whose id is not a key must not authenticate")
	}
}

// TestPossessionRefusesAnUndecodableProof checks each half of the proof encoding must
// decode: a payload or signature that is not base64 is refused, and so is a proof whose
// signed payload is not the claims (a valid signature over junk proves possession of the
// key but asserts nothing about the request).
func TestPossessionRefusesAnUndecodableProof(t *testing.T) {
	leaf := mustIdentity(t)
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	inner := fixedAuth{prin: principalFor(leaf)}
	p := RequirePossession(inner, clk)

	// A valid signature over a payload that is not JSON claims.
	junk := []byte("not claims")
	signedJunk := base64.RawURLEncoding.EncodeToString(junk) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(leaf.priv, junk))

	// Claims that verify and decode but bind to a different request.
	other, err := SignProof(leaf, http.MethodPost, "/v1/Other", "n-other", clk.Now())
	if err != nil {
		t.Fatalf("SignProof: %v", err)
	}

	cases := []struct{ name, proof string }{
		{"payload is not base64", "!!!.aGk"},
		{"signature is not base64", base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".!!!"},
		{"payload is not the claims", signedJunk},
		{"bound to another request", other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.Authenticate(proofReq(t, http.MethodGet, "/v1/Widget", tc.proof)); err == nil {
				t.Fatalf("proof %q must not authenticate", tc.proof)
			}
		})
	}
}

// TestTLSPossessionIgnoresANonEd25519Certificate checks the channel-binding path refuses a
// verified client certificate whose key is not the leaf key's algorithm at all. The
// handshake proved possession of some key, but not of this one.
func TestTLSPossessionIgnoresANonEd25519Certificate(t *testing.T) {
	leaf := mustIdentity(t)
	r, err := http.NewRequest(http.MethodGet, "/v1/Widget", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.TLS = &tls.ConnectionState{
		VerifiedChains:   [][]*x509.Certificate{{{}}},
		PeerCertificates: []*x509.Certificate{{PublicKey: &rsa.PublicKey{}}},
	}
	if tlsProvesPossession(r, leaf.Public()) {
		t.Error("a certificate carrying another key type must not prove possession of the leaf key")
	}
}

// TestNameConstraintsRefuseAnUnusableNamespace checks the constraint set is validated
// before a name is derived from it, so an unusable namespace fails here rather than at the
// provider with a name already half-registered.
func TestNameConstraintsRefuseAnUnusableNamespace(t *testing.T) {
	id := mustIdentity(t)
	cases := []struct {
		name string
		c    Constraints
		want string
	}{
		{"empty charset", Constraints{MaxLen: 30}, "empty charset"},
		{"inverted length window", Constraints{Charset: "abc", MinLen: 40, MaxLen: 30}, "below minLen"},
		{"no room for a suffix", Constraints{Charset: "abc", MinLen: 1, MaxLen: 4}, "suffix floor"},
		{
			"separator inside the charset",
			Constraints{Charset: "abc-", Separator: "-", MinLen: 1, MaxLen: 30},
			"overlaps the charset",
		},
		{
			"lead letter with no letters",
			Constraints{Charset: "0123456789", Separator: "-", MinLen: 1, MaxLen: 30, LeadLetter: true},
			"no leading letters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.valid(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("valid() = %v, want it to mention %q", err, tc.want)
			}
			if _, err := ExternalName(id.Public(), "flynn", "fly-app", tc.c); err == nil {
				t.Error("ExternalName must refuse an unusable constraint set")
			}
			if _, err := ResolveName(id, "flynn", "fly-app", "", tc.c); err == nil {
				t.Error("ResolveName must refuse an unusable constraint set")
			}
		})
	}
}

// TestExternalNameRefusesAKeyThatIsNotAKey checks a public key of the wrong size derives
// nothing: a name derived from a truncated key would not be the identity's name.
func TestExternalNameRefusesAKeyThatIsNotAKey(t *testing.T) {
	if _, err := ExternalName(ed25519.PublicKey("short"), "flynn", "fly-app", DNSName(30)); err == nil {
		t.Fatal("a key of the wrong size must derive no name")
	}
}

// TestExternalNameRefusesABaseThatCannotFit checks a base that is invalid in the namespace,
// or one that leaves no room for the derived suffix, is an error rather than a name the
// provider would reject.
func TestExternalNameRefusesABaseThatCannotFit(t *testing.T) {
	id := mustIdentity(t)
	c := DNSName(20)

	if _, err := id.ExternalName("Flynn_Agent", "fly-app", c); err == nil {
		t.Error("a base with characters outside the charset must be refused")
	}
	if _, err := id.ExternalName(strings.Repeat("a", 15), "fly-app", c); err == nil {
		t.Error("a base leaving no room for the derived suffix must be refused")
	}
}

// TestResolveNameRefusesAnInvalidOverride checks an explicit name is validated against the
// namespace before it is used, so a bad override fails here rather than at the provider.
func TestResolveNameRefusesAnInvalidOverride(t *testing.T) {
	id := mustIdentity(t)
	if _, err := ResolveName(id, "flynn", "fly-app", "-not-a-dns-label-", DNSName(30)); err == nil {
		t.Fatal("an invalid override must be refused")
	}
}

// TestValidateNamesTheRuleItRefusedOn checks each clause of the name check reports itself,
// so an operator learns which rule an override broke.
func TestValidateNamesTheRuleItRefusedOn(t *testing.T) {
	c := DNSName(20)
	c.MinLen = 3
	cases := []struct{ name, want string }{
		{"ab", "length"},
		{strings.Repeat("a", 21), "length"},
		{"has_underscore", "not allowed"},
		{"9leading", "begin with a letter"},
		{"-lead", "begin with a letter"},
		{"trail-", "must not begin or end"},
	}
	for _, tc := range cases {
		err := c.Validate(tc.name)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	if err := c.Validate("flynn-agent"); err != nil {
		t.Errorf("Validate of a well-formed name = %v, want nil", err)
	}
}

// TestDelegationAuthenticatorRefusesEveryBadToken checks the bridge from the token layer to
// the request model fails closed: no bearer token, a token that does not verify, and a
// token from another issuer are all simply unauthenticated, with no partial authority.
func TestDelegationAuthenticatorRefusesEveryBadToken(t *testing.T) {
	issuer, other, holder := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	future := now.Add(time.Hour)

	foreign, err := Issue(other, holder.Public(), ScopeOperator, capability.AllowAll(), future)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	a := NewDelegationAuthenticator(issuer.Public(), clock.NewManual(now))

	for _, tok := range []string{"", "not-a-token", foreign} {
		r, err := http.NewRequest(http.MethodGet, "/v1/Widget", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		if _, err := a.Authenticate(r); err == nil {
			t.Errorf("token %q must not authenticate", tok)
		}
	}

	// The same authenticator admits a token its own issuer minted, so the refusals above
	// are the rule doing its job rather than the authenticator refusing everything.
	good, err := Issue(issuer, holder.Public(), ScopeOperator, capability.AllowAll(), future)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	r, err := http.NewRequest(http.MethodGet, "/v1/Widget", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+good)
	prin, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if prin.ID != holder.ID() {
		t.Errorf("principal = %q, want the token's leaf audience %q", prin.ID, holder.ID())
	}
}

// TestTokenDecodeRejectsNonJSON checks a value that is valid base64 but not a token is
// reported as an unparsable token rather than an empty one.
func TestTokenDecodeRejectsNonJSON(t *testing.T) {
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("plain text"))
	if _, err := decodeToken(notJSON); err == nil || !strings.Contains(err.Error(), "parse token") {
		t.Fatalf("decodeToken error = %v, want a parse error", err)
	}
	// A block that is valid JSON but not a block object fails at parse, not at decode.
	if _, err := parseBlock(json.RawMessage(`["not", "a", "block"]`)); err == nil {
		t.Fatal("parseBlock must refuse a payload that is not a block")
	}
}
