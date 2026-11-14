package approval_test

import (
	"context"
	"crypto/ed25519"
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// fixedPolicy is a Policy that answers every action with one count.
type fixedPolicy int

func (p fixedPolicy) Required(dispatch.Action) int { return int(p) }

// TestFailClosedVerifier proves the verifier grants only on a positive match: a negative
// quorum, an empty keyring, a zero approval and an unregistered key all deny. A quorum of
// zero grants by design (it is how a policy says "no approval needed") and is pinned so
// changing it is a decision.
func TestFailClosedVerifier(t *testing.T) {
	ctx := context.Background()
	want := baseEnv("deploy", "run-1", "n1")

	t.Run("a negative quorum denies", func(t *testing.T) {
		s, pub := newSigner(t, "alice")
		v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
		a := sign(t, s, want)
		rapid.Check(t, func(rt *rapid.T) {
			n := rapid.IntRange(math.MinInt32, -1).Draw(rt, "required")
			for _, presented := range [][]approval.Approval{nil, {a}} {
				dec, err := v.Check(ctx, want, presented, n)
				if err != nil {
					rt.Fatalf("check: %v", err)
				}
				if dec.Granted {
					rt.Fatalf("a requirement of %d was granted with %d approvals", n, len(presented))
				}
				if dec.Reason == "" {
					rt.Fatal("a denial carries no reason")
				}
			}
		})
	})
	t.Run("a zero quorum grants by design", func(t *testing.T) {
		v, _ := newVerifier(t, approval.NewKeyring(), "")
		dec, err := v.Check(ctx, want, nil, 0)
		if err != nil || !dec.Granted {
			t.Fatalf("a zero requirement must grant (it means no approval is needed): %+v %v", dec, err)
		}
	})
	t.Run("a zero keyring authorizes nobody", func(t *testing.T) {
		s, _ := newSigner(t, "alice")
		var zero approval.Keyring
		v, _ := newVerifier(t, &zero, "")
		dec, err := v.Check(ctx, want, []approval.Approval{sign(t, s, want)}, 1)
		if err != nil || dec.Granted {
			t.Fatalf("a zero keyring granted: %+v %v", dec, err)
		}
	})
	t.Run("a zero approval never counts", func(t *testing.T) {
		_, pub := newSigner(t, "alice")
		v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
		dec, err := v.Check(ctx, want, []approval.Approval{{}, {}}, 1)
		if err != nil || dec.Granted {
			t.Fatalf("zero approvals granted: %+v %v", dec, err)
		}
	})
	t.Run("a key the ring does not hold never counts", func(t *testing.T) {
		s, _ := newSigner(t, "mallory")
		_, pub := newSigner(t, "alice")
		v, _ := newVerifier(t, keyringWith(t, map[string]ed25519.PublicKey{"alice": pub}), "")
		dec, err := v.Check(ctx, want, []approval.Approval{sign(t, s, want)}, 1)
		if err != nil || dec.Granted {
			t.Fatalf("an unregistered key granted: %+v %v", dec, err)
		}
	})
	t.Run("the keyring refuses an empty id and a malformed key", func(t *testing.T) {
		_, pub := newSigner(t, "alice")
		kr := approval.NewKeyring()
		if err := kr.Add("", pub); err == nil {
			t.Error("an empty key id was registered")
		}
		for _, bad := range []ed25519.PublicKey{nil, {}, pub[:len(pub)-1], append(append(ed25519.PublicKey{}, pub...), 0)} {
			if err := kr.Add("k", bad); err == nil {
				t.Errorf("a %d-byte key was registered", len(bad))
			}
		}
	})
}

// TestFailClosedGate proves the gate refuses what it cannot positively authorize: a
// negative requirement from the policy is Forbidden and the work does not run, and a
// presented approval that does not verify is Forbidden. An action the policy does not
// list requires nothing by design (a host opts privileged actions in), pinned here.
func TestFailClosedGate(t *testing.T) {
	ctx := capability.WithPrincipal(context.Background(), "run-1")
	newGov := func(t *testing.T, p approval.Policy) *dispatch.Dispatcher {
		t.Helper()
		v, _ := newVerifier(t, approval.NewKeyring(), "host-A")
		return dispatch.New(dispatch.WithHook(approval.NewGate(p, v, approval.WithGateHost("host-A"))))
	}

	t.Run("a negative requirement is forbidden and the work does not run", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			n := rapid.IntRange(math.MinInt32, -1).Draw(rt, "required")
			ran := false
			err := newGov(t, fixedPolicy(n)).Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran))
			if ran || err == nil {
				rt.Fatalf("a requirement of %d admitted the action", n)
			}
			if got := fault.Classify(err); got != fault.Forbidden {
				rt.Fatalf("a requirement of %d is %s, want forbidden", n, got)
			}
		})
	})
	t.Run("a zero approval presented to a positive requirement is forbidden", func(t *testing.T) {
		ran := false
		err := newGov(t, fixedPolicy(1)).Govern(approval.Into(ctx, approval.Approval{}), dispatch.Action{Name: "deploy"}, ranWork(&ran))
		if ran || fault.Classify(err) != fault.Forbidden {
			t.Fatalf("ran=%v err=%v, want forbidden", ran, err)
		}
	})
	t.Run("a positive requirement with nothing presented needs approval", func(t *testing.T) {
		ran := false
		err := newGov(t, fixedPolicy(1)).Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran))
		if ran || fault.Classify(err) != fault.NeedsApproval {
			t.Fatalf("ran=%v err=%v, want needs-approval", ran, err)
		}
	})
	t.Run("an unlisted action and a nil table require nothing by design", func(t *testing.T) {
		for name, p := range map[string]approval.Policy{
			"empty": approval.Requirements{},
			"nil":   approval.Requirements(nil),
		} {
			ran := false
			if err := newGov(t, p).Govern(ctx, dispatch.Action{Name: "deploy"}, ranWork(&ran)); err != nil || !ran {
				t.Errorf("%s table: ran=%v err=%v; an unlisted action is admitted by design", name, ran, err)
			}
		}
	})
}
