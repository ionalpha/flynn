package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/iotest"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// TestRequireApprovalFlagAccumulates: the flag is repeatable, so naming two actions gates
// two, and the second occurrence does not overwrite the first the way a plain string flag
// would. A blank occurrence is dropped rather than becoming an action named the empty
// string, which would list a requirement nothing could ever satisfy.
func TestRequireApprovalFlagAccumulates(t *testing.T) {
	var list stringList
	for _, v := range []string{"shell", "  ", "net.fetch", "shell"} {
		if err := list.Set(v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
	}
	if got := list.String(); got != "shell,net.fetch,shell" {
		t.Fatalf("accumulated %q, want every non-blank occurrence in order", got)
	}
	// The policy collapses the duplicate to one requirement. Repeating a flag is not how
	// anyone means to ask for a two-signature quorum.
	policy := approvalPolicy(list.values)
	if policy["shell"] != 1 {
		t.Fatalf("shell requires %d signatures, want 1", policy["shell"])
	}
	gated := gatedActions(policy)
	if len(gated) != 2 || gated[0] != "net.fetch" || gated[1] != "shell" {
		t.Fatalf("gated actions = %v, want the two distinct names, sorted", gated)
	}
	// A nil list reads as the empty string rather than panicking, since the flag package
	// calls String on the zero value while building its usage output.
	var empty *stringList
	if empty.String() != "" {
		t.Fatal("the zero flag value did not render as empty")
	}
}

// TestApprovalHostNeverResolvesEmpty: a machine that will not name itself falls back to a
// constant rather than to the empty string. The verifier reads an empty host as "valid on
// any host", so a broken hostname would silently widen every approval the run minted.
func TestApprovalHostNeverResolvesEmpty(t *testing.T) {
	if got := approvalHost(func() (string, error) { return "box-7", nil }); got != "box-7" {
		t.Fatalf("host = %q, want the name the machine gave", got)
	}
	if got := approvalHost(func() (string, error) { return "", errors.New("no hostname") }); got != "localhost" {
		t.Fatalf("a failed lookup resolved to %q, want the localhost fallback", got)
	}
	// A lookup that succeeds and hands back nothing is the same hazard as one that fails.
	if got := approvalHost(func() (string, error) { return "   ", nil }); got != "localhost" {
		t.Fatalf("a blank hostname resolved to %q, want the localhost fallback", got)
	}
}

// TestApprovalIdentityRefusesADegradedKey: the run's approver is minted from real entropy
// or not at all. An approval is a signature over what was authorized, so one signed with a
// key from a failing source would claim a binding it does not have; the run is refused
// rather than gated by a key nobody should trust.
func TestApprovalIdentityRefusesADegradedKey(t *testing.T) {
	signer, keyring, err := approvalIdentity(iotest.ErrReader(errors.New("no entropy")))
	if err == nil {
		t.Fatal("a failing entropy source still minted an approver")
	}
	if signer != nil || keyring != nil {
		t.Fatal("a refused identity handed back half of itself")
	}

	// The two halves are one identity: the keyring trusts exactly the key the signer holds,
	// so an approval the run mints verifies against the keyring the run checks with.
	signer, keyring, err = approvalIdentity(bytes.NewReader(bytes.Repeat([]byte{7}, 128)))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if signer.KeyID() != approvalKeyID {
		t.Fatalf("signer key id = %q, want %q", signer.KeyID(), approvalKeyID)
	}
	appr, err := signer.Sign(approval.Envelope{Action: "shell", Nonce: "n1"})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	dec, err := approval.NewVerifier(keyring, approval.NewMemStore()).Check(
		context.Background(), approval.Envelope{Action: "shell", Nonce: "n1"}, []approval.Approval{appr}, 1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !dec.Granted {
		t.Fatalf("the run's own keyring did not trust the run's own signature: %s", dec.Reason)
	}
}

// TestNoPolicyBuildsNoStack: naming no actions builds nothing at all, so an ungated run
// carries no gate, no signer and no key. The default install pays nothing for a mechanism
// it has not turned on, and a nil stack still answers every method.
func TestNoPolicyBuildsNoStack(t *testing.T) {
	stack, err := newApprovalStack(nil, spine.NewMemoryLog(), "run-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if stack != nil {
		t.Fatal("a run that listed no actions was given an approval stack")
	}
	if opts := stack.options(nil); opts != nil {
		t.Fatalf("a nil stack produced %d mission options", len(opts))
	}
	if spec := stack.spec(nil); spec != nil {
		t.Fatal("a nil stack produced a driver approval spec")
	}
}

// TestApprovalStackWiresBothForms: a policy builds one stack, and both shapes of it (the
// mission options for a directly-assembled executor, and the driver spec for a run routed
// through a Router) carry the same gate, signer and host. Two shapes that disagreed would
// mean a fan-out governed differently from a single conversation.
func TestApprovalStackWiresBothForms(t *testing.T) {
	stack, err := newApprovalStack([]string{"shell"}, spine.NewMemoryLog(), "run-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if stack == nil {
		t.Fatal("a policy naming an action built no stack")
	}
	if stack.host == "" {
		t.Fatal("the stack has no host, so an approval could not be bound to one")
	}

	// Without a prompter the gate is installed and nothing can resolve a pause, which is
	// the fail-closed non-interactive shape.
	if got := len(stack.options(nil)); got != 1 {
		t.Fatalf("options without a prompter = %d, want just the gate", got)
	}
	if got := len(stack.options(newStubPrompter(mission.ApprovalDecision{}))); got != 2 {
		t.Fatalf("options with a prompter = %d, want the gate and the prompter", got)
	}

	prompter := newStubPrompter(mission.ApprovalDecision{})
	spec := stack.spec(prompter)
	if spec == nil {
		t.Fatal("a live stack produced no driver approval spec")
	}
	if spec.Gate != stack.gate || spec.Signer != stack.signer || spec.Host != stack.host {
		t.Fatal("the driver spec and the mission options do not carry the same stack")
	}
	if spec.Prompter != mission.ApprovalPrompter(prompter) {
		t.Fatal("the driver spec dropped the prompter")
	}
}
