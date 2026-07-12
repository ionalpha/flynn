package main

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/signpolicy"
)

func testSigner(t *testing.T) extension.HostSigner {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := extension.NewEd25519HostSigner(priv)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	return s
}

// A policy name this binary does not know must be refused, not quietly replaced with one it
// does know. The operator asked for a policy that does not exist, so every policy is the wrong
// answer: substituting one binds the key to a purpose nobody chose, and a typo in a flag would
// be enough to do it.
func TestAnUnknownSignPolicyIsRefused(t *testing.T) {
	for _, choice := range []string{"", "solana", "Solana-Token", "solana-token ", "anything"} {
		p, err := signPolicyFor(choice, testSigner(t))
		if err == nil {
			t.Errorf("--sign-policy %q was accepted and bound %T; an unknown policy must be refused", choice, p)
		}
		if p != nil {
			t.Errorf("--sign-policy %q returned a policy (%T) alongside its error", choice, p)
		}
	}
}

// The error has to say what the operator may write instead, otherwise the refusal above just
// moves the problem: a policy that cannot be named is a policy that cannot be used.
func TestTheRefusalNamesTheKnownPolicies(t *testing.T) {
	_, err := signPolicyFor("nope", testSigner(t))
	if err == nil {
		t.Fatal("an unknown policy was accepted")
	}
	for _, name := range []string{"solana-token", "any"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name the %q policy: %v", name, err)
		}
	}
}

// The names that do exist must map to the policy they claim to be. "solana-token" reading the
// transaction, and "any" signing whatever it is handed, are different enough that binding a key
// to the wrong one is the whole failure this flag exists to prevent.
func TestTheKnownPoliciesMapToWhatTheyName(t *testing.T) {
	signer := testSigner(t)

	p, err := signPolicyFor("solana-token", signer)
	if err != nil {
		t.Fatalf("solana-token: %v", err)
	}
	sol, ok := p.(signpolicy.Solana)
	if !ok {
		t.Fatalf("solana-token bound %T, not the Solana policy", p)
	}
	if !bytes.Equal(sol.Payer, signer.Public()) {
		t.Error("the Solana policy was bound to a different key than the one being granted, so it would approve somebody else's transaction")
	}

	p, err = signPolicyFor("any", signer)
	if err != nil {
		t.Fatalf("any: %v", err)
	}
	if _, ok := p.(extension.AnyPayload); !ok {
		t.Fatalf("any bound %T, not the blind-signing policy", p)
	}
}
