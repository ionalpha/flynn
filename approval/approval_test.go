package approval_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// fixedTime is the manual clock's start, so the validity window is deterministic.
var fixedTime = time.Unix(1_000_000, 0).UTC()

func hourLater() int64  { return fixedTime.Add(time.Hour).UnixNano() }
func hourBefore() int64 { return fixedTime.Add(-time.Hour).UnixNano() }

// newSigner mints an Ed25519 signer and returns it with its public key.
func newSigner(t *testing.T, keyID string) (*approval.Ed25519Signer, ed25519.PublicKey) {
	t.Helper()
	s, pub, err := approval.GenerateEd25519Signer(keyID, rand.Reader)
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	return s, pub
}

// keyringWith builds a keyring registering each signer's public key.
func keyringWith(t *testing.T, entries map[string]ed25519.PublicKey) *approval.Keyring {
	t.Helper()
	kr := approval.NewKeyring()
	for id, pub := range entries {
		if err := kr.Add(id, pub); err != nil {
			t.Fatalf("keyring add %s: %v", id, err)
		}
	}
	return kr
}

// sign signs env with s, failing the test on error.
func sign(t *testing.T, s approval.Signer, env approval.Envelope) approval.Approval {
	t.Helper()
	a, err := s.Sign(env)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return a
}

// baseEnv is a well-bound, in-window envelope for action with a unique nonce.
func baseEnv(action, principal, nonce string) approval.Envelope {
	return approval.Envelope{
		Action:    action,
		Principal: principal,
		Nonce:     nonce,
		NotBefore: 0,
		Expiry:    hourLater(),
	}
}

func newVerifier(t *testing.T, kr *approval.Keyring, host string) (*approval.Verifier, *approval.MemStore) {
	t.Helper()
	ns := approval.NewMemStore()
	v := approval.NewVerifier(kr, ns,
		approval.WithClock(clock.NewManual(fixedTime)),
		approval.WithHost(host))
	return v, ns
}

func TestSingleApprovalGrants(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
	a := sign(t, s, baseEnv("deploy", "run-1", "n1"))

	dec, err := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"), []approval.Approval{a}, 1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !dec.Granted {
		t.Fatalf("expected granted, got denial: %s", dec.Reason)
	}
	if len(dec.KeyIDs) != 1 || dec.KeyIDs[0] != "alice" {
		t.Fatalf("expected alice to be recorded, got %v", dec.KeyIDs)
	}
}

func TestNoApprovalIsNotGranted(t *testing.T) {
	v, _ := newVerifier(t, approval.NewKeyring(), "")
	dec, err := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"), nil, 1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if dec.Granted {
		t.Fatal("expected denial with no approvals presented")
	}
}

func TestForgedSignatureRejected(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
	a := sign(t, s, baseEnv("deploy", "run-1", "n1"))
	a.Signature[0] ^= 0xff // tamper

	dec, _ := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"), []approval.Approval{a}, 1)
	if dec.Granted {
		t.Fatal("a tampered signature must not be granted")
	}
}

func TestUnauthorizedKeyRejected(t *testing.T) {
	s, _ := newSigner(t, "mallory") // mallory's key is NOT in the ring
	v, _ := newVerifier(t, approval.NewKeyring(), "")
	a := sign(t, s, baseEnv("deploy", "run-1", "n1"))

	dec, _ := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"), []approval.Approval{a}, 1)
	if dec.Granted {
		t.Fatal("a signature from an unauthorized key must not be granted")
	}
}

func TestApprovalCannotBeWidenedToAnotherAction(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
	// Alice approved "read", the run tries to use it for "deploy".
	a := sign(t, s, baseEnv("read", "run-1", "n1"))

	dec, _ := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"), []approval.Approval{a}, 1)
	if dec.Granted {
		t.Fatal("an approval for one action must not authorize another")
	}
}

func TestApprovalBoundToPrincipalScopeAndDetail(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")

	signed := baseEnv("deploy", "run-1", "n1")
	signed.Scope = state.Scope{Project: "p1"}
	signed.Detail = "prod"
	a := sign(t, s, signed)

	cases := []struct {
		name string
		want approval.Envelope
	}{
		{"wrong principal", mutate(signed, func(e *approval.Envelope) { e.Principal = "run-2" })},
		{"wrong scope", mutate(signed, func(e *approval.Envelope) { e.Scope = state.Scope{Project: "p2"} })},
		{"wrong detail", mutate(signed, func(e *approval.Envelope) { e.Detail = "staging" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, _ := v.Check(context.Background(), tc.want, []approval.Approval{a}, 1)
			if dec.Granted {
				t.Fatalf("%s: approval must not authorize a mismatched binding", tc.name)
			}
		})
	}

	// The exact binding is granted.
	dec, _ := v.Check(context.Background(), signed, []approval.Approval{a}, 1)
	if !dec.Granted {
		t.Fatalf("the exact binding should be granted, got: %s", dec.Reason)
	}
}

func TestExpiredAndNotYetValidRejected(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")

	expired := baseEnv("deploy", "run-1", "n1")
	expired.Expiry = hourBefore()
	future := baseEnv("deploy", "run-1", "n2")
	future.NotBefore = hourLater()

	for _, tc := range []struct {
		name string
		env  approval.Envelope
	}{{"expired", expired}, {"not yet valid", future}} {
		t.Run(tc.name, func(t *testing.T) {
			a := sign(t, s, tc.env)
			dec, _ := v.Check(context.Background(), tc.env, []approval.Approval{a}, 1)
			if dec.Granted {
				t.Fatalf("%s approval must not be granted", tc.name)
			}
		})
	}
}

func TestReplayRejectedAfterUse(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
	env := baseEnv("deploy", "run-1", "n1")
	a := sign(t, s, env)

	if dec, _ := v.Check(context.Background(), env, []approval.Approval{a}, 1); !dec.Granted {
		t.Fatalf("first use should be granted, got: %s", dec.Reason)
	}
	if dec, _ := v.Check(context.Background(), env, []approval.Approval{a}, 1); dec.Granted {
		t.Fatal("the same approval must not be granted twice (single-use nonce)")
	}
}

func TestNonceNotBurnedOnUnmetQuorum(t *testing.T) {
	alice, apub := newSigner(t, "alice")
	bob, bpub := newSigner(t, "bob")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": apub, "bob": bpub}), "")
	env := baseEnv("deploy", "run-1", "n1")
	aliceApproval := sign(t, alice, env)

	// Two signatures required, only alice's presented: denied, and her nonce must
	// not be spent, so a later complete quorum still works.
	if dec, _ := v.Check(context.Background(), env, []approval.Approval{aliceApproval}, 2); dec.Granted {
		t.Fatal("one of two required signatures must not be granted")
	}
	bobEnv := baseEnv("deploy", "run-1", "n2")
	bobApproval := sign(t, bob, bobEnv)
	// Present alice (n1, must still be unspent) + bob (n2): now quorum is met.
	dec, _ := v.Check(context.Background(), env, []approval.Approval{aliceApproval, bobApproval}, 2)
	if !dec.Granted {
		t.Fatalf("a complete quorum should be granted; alice's nonce was wrongly burned? reason: %s", dec.Reason)
	}
}

func TestQuorumNeedsDistinctApprovers(t *testing.T) {
	alice, apub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": apub}), "")

	// Alice signs two different envelopes; both are valid but from one key, so they
	// cannot satisfy a 2-of-N requirement.
	a1 := sign(t, alice, baseEnv("deploy", "run-1", "n1"))
	a2 := sign(t, alice, baseEnv("deploy", "run-1", "n2"))
	dec, _ := v.Check(context.Background(), baseEnv("deploy", "run-1", "n1"),
		[]approval.Approval{a1, a2}, 2)
	if dec.Granted {
		t.Fatal("two signatures from one approver must not satisfy a two-approver quorum")
	}
}

func TestHostBinding(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "host-A")

	otherHost := baseEnv("deploy", "run-1", "n1")
	otherHost.Host = "host-B"
	if dec, _ := v.Check(context.Background(), otherHost, []approval.Approval{sign(t, s, otherHost)}, 1); dec.Granted {
		t.Fatal("an approval bound to another host must not be granted here")
	}

	anyHost := baseEnv("deploy", "run-1", "n2") // Host == "" means any host
	want := anyHost
	want.Host = "host-A"
	if dec, _ := v.Check(context.Background(), want, []approval.Approval{sign(t, s, anyHost)}, 1); !dec.Granted {
		t.Fatalf("a host-agnostic approval should be granted on this host, got: %s", dec.Reason)
	}
}

// --- gate (dispatch waist) integration -------------------------------------

type recordingSink struct{ decisions []approval.Decision }

func (r *recordingSink) Record(_ context.Context, d approval.Decision) error {
	r.decisions = append(r.decisions, d)
	return nil
}

// ranWork is a dispatch work closure recording whether it executed.
func ranWork(ran *bool) func(context.Context) (dispatch.Metering, error) {
	return func(context.Context) (dispatch.Metering, error) { *ran = true; return dispatch.Metering{}, nil }
}

func TestGateFailsClosedAtTheWaist(t *testing.T) {
	s, pub := newSigner(t, "alice")
	kr := keyringWith(t, map[string]ed25519.PublicKey{"alice": pub})
	v, _ := newVerifier(t, kr, "host-A")
	sink := &recordingSink{}
	gate := approval.NewGate(approval.Requirements{"deploy": 1}, v,
		approval.WithSink(sink), approval.WithGateHost("host-A"))
	d := dispatch.New(dispatch.WithHook(gate))

	// A privileged action with no approval is refused, and the work never runs.
	ran := false
	ctx := capability.WithPrincipal(context.Background(), "run-1")
	err := d.Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran))
	if got := fault.Classify(err); got != fault.NeedsApproval {
		t.Fatalf("expected NeedsApproval, got %s: %v", got, err)
	}
	if ran {
		t.Fatal("the privileged action ran without approval")
	}

	// A non-privileged action is admitted with no approval.
	ran = false
	if err := d.Govern(ctx, dispatch.Action{Name: "read"}, ranWork(&ran)); err != nil {
		t.Fatalf("non-privileged action should be admitted: %v", err)
	}
	if !ran {
		t.Fatal("non-privileged work should have run")
	}

	// With a valid approval on the context, the privileged action runs.
	ran = false
	want := approval.Binding(ctx, dispatch.Action{Name: "deploy"}, "host-A")
	want.Nonce, want.Expiry = "n1", hourLater()
	ctx = approval.Into(ctx, sign(t, s, want))
	if err := d.Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran)); err != nil {
		t.Fatalf("approved action should run: %v", err)
	}
	if !ran {
		t.Fatal("approved work should have run")
	}

	// The sink saw a denial then a grant.
	if len(sink.decisions) != 2 {
		t.Fatalf("expected 2 recorded decisions, got %d", len(sink.decisions))
	}
	if sink.decisions[0].Granted || !sink.decisions[1].Granted {
		t.Fatalf("expected [denied, granted], got [%v, %v]", sink.decisions[0].Granted, sink.decisions[1].Granted)
	}
}

func TestGateRejectsInvalidApprovalAsForbidden(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "host-A")
	gate := approval.NewGate(approval.Requirements{"deploy": 1}, v, approval.WithGateHost("host-A"))
	d := dispatch.New(dispatch.WithHook(gate))

	ctx := capability.WithPrincipal(context.Background(), "run-1")
	// An approval for the wrong action: presented, but does not verify for "deploy".
	wrong := approval.Binding(ctx, dispatch.Action{Name: "read"}, "host-A")
	wrong.Nonce, wrong.Expiry = "n1", hourLater()
	ctx = approval.Into(ctx, sign(t, s, wrong))

	ran := false
	err := d.Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran))
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("expected Forbidden for a presented-but-invalid approval, got %s: %v", got, err)
	}
	if ran {
		t.Fatal("an action with an invalid approval must not run")
	}
}

// TestQuorumProperty is the rigor property: a quorum of `required` is granted
// exactly when at least `required` distinct authorized approvers present valid,
// in-window, correctly-bound, unspent approvals, and never with fewer; and a
// granted set's nonces are spent so it cannot be replayed.
func TestQuorumProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		required := rapid.IntRange(1, 5).Draw(rt, "required")
		signers := rapid.IntRange(0, 6).Draw(rt, "signers")

		kr := approval.NewKeyring()
		v, _ := newVerifier(t, kr, "")
		env := baseEnv("deploy", "run-1", "base")

		approvals := make([]approval.Approval, 0, signers)
		for i := range signers {
			id := "signer-" + string(rune('a'+i))
			s, pub, err := approval.GenerateEd25519Signer(id, rand.Reader)
			if err != nil {
				rt.Fatalf("gen: %v", err)
			}
			if err := kr.Add(id, pub); err != nil {
				rt.Fatalf("add: %v", err)
			}
			e := env
			e.Nonce = "n-" + id
			a, err := s.Sign(e)
			if err != nil {
				rt.Fatalf("sign: %v", err)
			}
			approvals = append(approvals, a)
		}

		dec, err := v.Check(context.Background(), env, approvals, required)
		if err != nil {
			rt.Fatalf("check: %v", err)
		}
		wantGranted := signers >= required
		if dec.Granted != wantGranted {
			rt.Fatalf("granted=%v, want %v (signers=%d, required=%d)", dec.Granted, wantGranted, signers, required)
		}
		if dec.Granted {
			// A granted set is now spent: an identical re-check must be denied.
			if again, _ := v.Check(context.Background(), env, approvals, required); again.Granted {
				rt.Fatal("a granted approval set must not be replayable")
			}
		}
	})
}

// mutate returns a copy of e with fn applied, for building near-miss bindings.
func mutate(e approval.Envelope, fn func(*approval.Envelope)) approval.Envelope {
	fn(&e)
	return e
}
