package controlplane

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
)

// A fixed instant so token expiry is deterministic and never reads the wall clock.
var dgNow = time.Unix(1_900_000_000, 0).UTC()

func dgIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

// sealBlock signs an arbitrary block with a chosen key, so a test can craft a token the
// public Issue/Attenuate API would refuse and prove Verify rejects it on its own.
func sealBlock(t *testing.T, blk capBlock, signer *Identity) sealedBlock {
	t.Helper()
	sb, err := sign(blk, signer.priv)
	if err != nil {
		t.Fatalf("sign block: %v", err)
	}
	return sb
}

func craft(t *testing.T, blocks ...sealedBlock) string {
	t.Helper()
	tok, err := encodeToken(capToken{Blocks: blocks})
	if err != nil {
		t.Fatalf("encode token: %v", err)
	}
	return tok
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, err := Issue(issuer, holder.Public(), ScopeOperator, capability.NewGrant("a", "b"), dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	auth, err := Verify(tok, issuer.Public(), dgNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if auth.Subject != holder.ID() {
		t.Fatalf("subject = %q, want %q", auth.Subject, holder.ID())
	}
	if auth.Scope != ScopeOperator {
		t.Fatalf("scope = %v, want operator", auth.Scope)
	}
	if !auth.Grant.Allows("a") || !auth.Grant.Allows("b") || auth.Grant.Allows("c") {
		t.Fatalf("grant = %v, want exactly {a,b}", auth.Grant.Actions())
	}
}

func TestAttenuateNarrowsScopeAndActions(t *testing.T) {
	issuer, h1, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	root, err := Issue(issuer, h1.Public(), ScopeOperator, capability.AllowAll(), dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Operator + AllowAll narrowed to read + just {a}.
	att, err := Attenuate(root, h1, h2.Public(), ScopeRead, []string{"a"}, dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("attenuate: %v", err)
	}
	auth, err := Verify(att, issuer.Public(), dgNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if auth.Subject != h2.ID() {
		t.Fatalf("subject = %q, want the new audience %q", auth.Subject, h2.ID())
	}
	if auth.Scope != ScopeRead {
		t.Fatalf("scope = %v, want read after narrowing", auth.Scope)
	}
	if !auth.Grant.Allows("a") || auth.Grant.Allows("b") {
		t.Fatalf("grant = %v, want exactly {a}", auth.Grant.Actions())
	}
}

func TestMultiHopAttenuationIntersects(t *testing.T) {
	issuer, h1, h2, h3 := dgIdentity(t), dgIdentity(t), dgIdentity(t), dgIdentity(t)
	root, _ := Issue(issuer, h1.Public(), ScopeAdmin, capability.NewGrant("a", "b", "c"), dgNow.Add(time.Hour))
	hop1, err := Attenuate(root, h1, h2.Public(), ScopeOperator, []string{"a", "b"}, dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("hop1: %v", err)
	}
	hop2, err := Attenuate(hop1, h2, h3.Public(), ScopeRead, []string{"a"}, dgNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("hop2: %v", err)
	}
	auth, err := Verify(hop2, issuer.Public(), dgNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if auth.Scope != ScopeRead || !auth.Grant.Allows("a") || auth.Grant.Allows("b") || auth.Grant.Allows("c") {
		t.Fatalf("effective authority = scope %v grant %v, want read {a}", auth.Scope, auth.Grant.Actions())
	}
}

func TestVerifyRejectsForgedIssuer(t *testing.T) {
	issuer, attacker, holder := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), dgNow.Add(time.Hour))
	if _, err := Verify(tok, attacker.Public(), dgNow); err == nil {
		t.Fatal("a token must not verify against a key other than its issuer's")
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), dgNow.Add(time.Hour))
	dec, err := decodeToken(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	dec.Blocks[0].Payload[len(dec.Blocks[0].Payload)-2] ^= 0xff // flip a byte of the signed bytes
	tampered, _ := encodeToken(dec)
	if _, err := Verify(tampered, issuer.Public(), dgNow); err == nil {
		t.Fatal("a tampered payload must fail signature verification")
	}
}

func TestVerifyRejectsWidenedAction(t *testing.T) {
	issuer, h1, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	// Root grants only {a}. A hand-crafted attenuation (validly signed by h1) lists {b},
	// an action the parent never held: Verify must reject the widening.
	rootBlk := capBlock{Scope: ScopeOperator, Actions: []string{"a"}, Audience: h1.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	widenBlk := capBlock{Scope: ScopeOperator, Actions: []string{"b"}, Audience: h2.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	tok := craft(t, sealBlock(t, rootBlk, issuer), sealBlock(t, widenBlk, h1))
	if _, err := Verify(tok, issuer.Public(), dgNow); err == nil {
		t.Fatal("an attenuation that lists an action the parent lacks must be rejected")
	}
}

func TestVerifyRejectsScopeEscalation(t *testing.T) {
	issuer, h1, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	rootBlk := capBlock{Scope: ScopeRead, AllowAll: true, Audience: h1.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	escalate := capBlock{Scope: ScopeAdmin, Actions: []string{"a"}, Audience: h2.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	tok := craft(t, sealBlock(t, rootBlk, issuer), sealBlock(t, escalate, h1))
	if _, err := Verify(tok, issuer.Public(), dgNow); err == nil {
		t.Fatal("an attenuation with a higher scope than its parent must be rejected")
	}
}

func TestVerifyRejectsAllowAllAttenuation(t *testing.T) {
	issuer, h1, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t)
	rootBlk := capBlock{Scope: ScopeOperator, Actions: []string{"a"}, Audience: h1.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	allowAll := capBlock{Scope: ScopeOperator, AllowAll: true, Audience: h2.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	tok := craft(t, sealBlock(t, rootBlk, issuer), sealBlock(t, allowAll, h1))
	if _, err := Verify(tok, issuer.Public(), dgNow); err == nil {
		t.Fatal("an unrestricted (AllowAll) attenuation block must be rejected")
	}
}

func TestVerifyRejectsWrongSigner(t *testing.T) {
	issuer, h1, h2, imposter := dgIdentity(t), dgIdentity(t), dgIdentity(t), dgIdentity(t)
	rootBlk := capBlock{Scope: ScopeOperator, AllowAll: true, Audience: h1.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	// A block validly narrowing, but signed by an imposter rather than h1 (the audience
	// the previous block named): the chain of custody is broken and must be rejected.
	blk := capBlock{Scope: ScopeRead, Actions: []string{"a"}, Audience: h2.Public(), NotAfter: dgNow.Add(time.Hour).Unix()}
	tok := craft(t, sealBlock(t, rootBlk, issuer), sealBlock(t, blk, imposter))
	if _, err := Verify(tok, issuer.Public(), dgNow); err == nil {
		t.Fatal("a block not signed by the previous block's audience must be rejected")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	issuer, holder := dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, holder.Public(), ScopeRead, capability.NewGrant("a"), dgNow.Add(-time.Second))
	if _, err := Verify(tok, issuer.Public(), dgNow); err == nil {
		t.Fatal("an expired token must be rejected")
	}
}

func TestAttenuateRejectsNonAudience(t *testing.T) {
	issuer, h1, other, h2 := dgIdentity(t), dgIdentity(t), dgIdentity(t), dgIdentity(t)
	tok, _ := Issue(issuer, h1.Public(), ScopeOperator, capability.AllowAll(), dgNow.Add(time.Hour))
	// `other` is not the token's leaf audience, so it cannot attenuate it.
	if _, err := Attenuate(tok, other, h2.Public(), ScopeRead, []string{"a"}, dgNow.Add(time.Hour)); err == nil {
		t.Fatal("only the current leaf audience may attenuate a token")
	}
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	issuer := dgIdentity(t)
	for _, bad := range []string{"", "not-base64-!!!", "Zm9v" /* base64("foo"), not a token */} {
		if _, err := Verify(bad, issuer.Public(), dgNow); err == nil {
			t.Fatalf("malformed token %q must be rejected", bad)
		}
	}
}

// TestIssueRejectsBadAudience guards the mint path: an audience that is not a valid
// Ed25519 public key is refused rather than producing an unusable token.
func TestIssueRejectsBadAudience(t *testing.T) {
	issuer := dgIdentity(t)
	if _, err := Issue(issuer, ed25519.PublicKey("short"), ScopeRead, capability.NewGrant("a"), dgNow.Add(time.Hour)); err == nil {
		t.Fatal("issuing to an invalid audience key must fail")
	}
}
