package approval_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
)

// errStore is the injected nonce-store failure the tests below assert on.
var errStore = errors.New("nonce store unreachable")

// manualClock is the same fixed time source the rest of the package's tests verify
// validity windows against, for verifiers built with a nonce store of their own.
func manualClock() clock.Clock { return clock.NewManual(fixedTime) }

// TestNewEd25519SignerRefusesAKeyItCouldNeverSignWith: a signer built over a
// malformed key, or with no key id, would fail at the moment an operator is waiting
// on an approval. It is refused when it is built instead.
func TestNewEd25519SignerRefusesAKeyItCouldNeverSignWith(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		keyID string
		priv  ed25519.PrivateKey
	}{
		{"no key id", "", priv},
		{"truncated key", "alice", priv[:16]},
		{"no key", "alice", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := approval.NewEd25519Signer(tc.keyID, tc.priv)
			if err == nil {
				t.Fatalf("NewEd25519Signer(%q, %d-byte key) succeeded", tc.keyID, len(tc.priv))
			}
			if s != nil {
				t.Error("a rejected signer was still returned")
			}
			if got := fault.Classify(err); got != fault.Terminal {
				t.Errorf("rejection class = %v, want Terminal (a different key, not a retry)", got)
			}
		})
	}
}

// TestEd25519SignerOverAnExistingKeyVerifies: a key held elsewhere (a token, a file
// an operator already has) signs approvals the verifier accepts, so the signer is
// not tied to keys this package minted.
func TestEd25519SignerOverAnExistingKeyVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := approval.NewEd25519Signer("alice", priv)
	if err != nil {
		t.Fatalf("NewEd25519Signer: %v", err)
	}
	if got := s.KeyID(); got != "alice" {
		t.Errorf("KeyID() = %q, want alice", got)
	}

	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
	env := baseEnv("deploy", "run-1", "n1")
	dec, err := v.Check(context.Background(), env, []approval.Approval{sign(t, s, env)}, 1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !dec.Granted {
		t.Fatalf("an approval from an existing key was denied: %s", dec.Reason)
	}
}

// TestGenerateEd25519SignerReportsAFailedKeygen: a randomness source that cannot be
// read must not yield a signer over a key nobody can reason about.
func TestGenerateEd25519SignerReportsAFailedKeygen(t *testing.T) {
	if _, _, err := approval.GenerateEd25519Signer("", rand.Reader); err == nil {
		t.Error("GenerateEd25519Signer accepted an empty key id")
	}

	broken := testkit.FaultyReader(rand.Reader, testkit.Always(errStore))
	s, pub, err := approval.GenerateEd25519Signer("alice", broken)
	if err == nil {
		t.Fatal("GenerateEd25519Signer succeeded over a randomness source that always fails")
	}
	if !errors.Is(err, errStore) {
		t.Errorf("error = %v, want it to wrap the injected read failure", err)
	}
	if s != nil || pub != nil {
		t.Error("a failed keygen still returned a signer or a public key")
	}
}

// TestKeyringRefusesAKeyThatCouldNeverVerify: a malformed public key in the ring
// would silently reject every signature from an approver who is in fact authorized.
func TestKeyringRefusesAKeyThatCouldNeverVerify(t *testing.T) {
	_, pub := newSigner(t, "alice")
	kr := approval.NewKeyring()

	if err := kr.Add("", pub); err == nil {
		t.Error("keyring accepted an approver with no key id")
	}
	if err := kr.Add("alice", pub[:8]); err == nil {
		t.Error("keyring accepted a truncated public key")
	}
	if err := kr.Add("alice", pub); err != nil {
		t.Fatalf("keyring refused a valid key: %v", err)
	}
}

// TestNonceStoreRefusesASecondUse pins the single-use property the whole replay
// defence rests on: the first Use commits, and every later one is Forbidden.
func TestNonceStoreRefusesASecondUse(t *testing.T) {
	ctx := context.Background()
	ns := approval.NewMemStore()

	if seen, err := ns.Seen(ctx, "n1"); err != nil || seen {
		t.Fatalf("Seen(n1) = %v, %v; want false, nil before any use", seen, err)
	}
	if err := ns.Use(ctx, "n1"); err != nil {
		t.Fatalf("first Use: %v", err)
	}
	if seen, err := ns.Seen(ctx, "n1"); err != nil || !seen {
		t.Fatalf("Seen(n1) = %v, %v; want true, nil after use", seen, err)
	}

	err := ns.Use(ctx, "n1")
	if !errors.Is(err, approval.ErrNonceUsed) {
		t.Fatalf("second Use = %v, want ErrNonceUsed", err)
	}
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Errorf("replay class = %v, want Forbidden", got)
	}
}

// failingNonces is a NonceStore whose reads or writes fail, modelling a durable
// store that is down. It is not a testkit injector because testkit wraps no such
// port; the two failures are separate so a test names the one it is forcing.
type failingNonces struct {
	seenErr error
	useErr  error
}

func (n failingNonces) Seen(context.Context, string) (bool, error) { return false, n.seenErr }

func (n failingNonces) Use(context.Context, string) error { return n.useErr }

// TestCheckFailsClosedWhenTheNonceStoreCannotBeRead: a store the verifier cannot
// consult cannot prove an approval is not a replay, so the check errors rather than
// granting on an unchecked nonce.
func TestCheckFailsClosedWhenTheNonceStoreCannotBeRead(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v := approval.NewVerifier(
		keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}),
		failingNonces{seenErr: errStore},
		approval.WithClock(manualClock()),
	)
	env := baseEnv("deploy", "run-1", "n1")

	dec, err := v.Check(context.Background(), env, []approval.Approval{sign(t, s, env)}, 1)
	if !errors.Is(err, errStore) {
		t.Fatalf("Check error = %v, want the injected store failure", err)
	}
	if dec.Granted {
		t.Fatal("an action was granted while the replay check was unavailable")
	}
}

// TestCheckDeniesWhenAContributingNonceIsSpentConcurrently: the quorum is confirmed
// against a peek, and the commit can still lose a race with another run. Losing it
// denies the action rather than admitting it on an approval already spent.
func TestCheckDeniesWhenAContributingNonceIsSpentConcurrently(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v := approval.NewVerifier(
		keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}),
		failingNonces{useErr: approval.ErrNonceUsed}, // Seen says fresh, Use says spent
		approval.WithClock(manualClock()),
	)
	env := baseEnv("deploy", "run-1", "n1")

	dec, err := v.Check(context.Background(), env, []approval.Approval{sign(t, s, env)}, 1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if dec.Granted {
		t.Fatal("an approval whose nonce was spent between the peek and the commit was granted")
	}
	if dec.Reason == "" {
		t.Error("a denial carries no reason for the audit record")
	}
	if len(dec.KeyIDs) != 0 {
		t.Errorf("a denial recorded contributors %v", dec.KeyIDs)
	}
}

// TestCheckGrantsAnActionThatNeedsNoApproval: a requirement of zero is the
// unprivileged action, admitted under the capability grant alone, and no nonce is
// spent proving it.
func TestCheckGrantsAnActionThatNeedsNoApproval(t *testing.T) {
	v, ns := newVerifier(t, approval.NewKeyring(), "")

	dec, err := v.Check(context.Background(), baseEnv("read", "run-1", "n1"), nil, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !dec.Granted {
		t.Fatalf("an action requiring no approval was denied: %s", dec.Reason)
	}
	if seen, err := ns.Seen(context.Background(), "n1"); err != nil || seen {
		t.Error("an unprivileged action spent a nonce")
	}
}

// TestGateBindsTheApprovalToTheTargetOnTheContext: WithDetail narrows the authority
// to a specific target, so an approval signed for one target does not authorize the
// same action against another.
func TestGateBindsTheApprovalToTheTargetOnTheContext(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "host-A")
	gate := approval.NewGate(approval.Requirements{"deploy": 1}, v, approval.WithGateHost("host-A"))
	d := dispatch.New(dispatch.WithHook(gate))

	base := capability.WithPrincipal(context.Background(), "run-1")
	staging := approval.WithDetail(base, "staging")
	production := approval.WithDetail(base, "prod")

	// Alice approved a deploy to staging only.
	want := approval.Binding(staging, dispatch.Action{Name: "deploy"}, "host-A")
	if want.Detail != "staging" {
		t.Fatalf("binding detail = %q, want the target bound on the context", want.Detail)
	}
	want.Nonce, want.Expiry = "n1", hourLater()
	approved := sign(t, s, want)

	// The same approval against production is refused: the target is signed.
	ran := false
	err := d.Govern(approval.Into(production, approved), dispatch.Action{Name: "deploy"}, ranWork(&ran))
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("deploy to prod on a staging approval: class %v, want Forbidden", got)
	}
	if ran {
		t.Fatal("a deploy ran against a target nobody approved")
	}

	// Against staging, the target it was signed for, it runs.
	if err := d.Govern(approval.Into(staging, approved), dispatch.Action{Name: "deploy"}, ranWork(&ran)); err != nil {
		t.Fatalf("the approved target was refused: %v", err)
	}
	if !ran {
		t.Fatal("the approved deploy did not run")
	}
}

// TestGateReportsAnUnavailableVerifierAsForbidden: a check that could not be made is
// not an admission. The action is refused, and refused as Forbidden rather than as a
// missing approval, because a nonce store that is down will not be fixed by
// presenting one.
func TestGateReportsAnUnavailableVerifierAsForbidden(t *testing.T) {
	s, pub := newSigner(t, "alice")
	v := approval.NewVerifier(
		keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}),
		failingNonces{seenErr: errStore},
		approval.WithClock(manualClock()),
		approval.WithHost("host-A"),
	)
	gate := approval.NewGate(approval.Requirements{"deploy": 1}, v, approval.WithGateHost("host-A"))
	d := dispatch.New(dispatch.WithHook(gate))

	ctx := capability.WithPrincipal(context.Background(), "run-1")
	want := approval.Binding(ctx, dispatch.Action{Name: "deploy"}, "host-A")
	want.Nonce, want.Expiry = "n1", hourLater()

	ran := false
	err := d.Govern(approval.Into(ctx, sign(t, s, want)), dispatch.Action{Name: "deploy"}, ranWork(&ran))
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("class = %v, want Forbidden when the verifier could not decide", got)
	}
	if ran {
		t.Fatal("a privileged action ran though its authorization could not be checked")
	}
}
