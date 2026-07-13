package capability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
)

// tier is a sandbox that only declares a containment level: the gate decides on the
// level alone, so nothing else about the sandbox is needed to test it.
type tier struct{ level sandbox.Containment }

func (t tier) Containment() sandbox.Containment { return t.level }

func (tier) Exec(context.Context, sandbox.Command) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, errors.New("capability_test: the tier sandbox runs nothing")
}
func (tier) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (tier) WriteFile(context.Context, string, []byte) error  { return nil }
func (tier) Glob(context.Context, string) ([]string, error)   { return nil, nil }
func (tier) Walk(context.Context, string) ([]string, error)   { return nil, nil }
func (tier) Close() error                                     { return nil }

var _ sandbox.Sandbox = tier{}

// TestContainmentGateRefusesWorkTheSandboxCannotContain is the no-downgrade rule at
// the waist: work is admitted only where the host can actually isolate it, and a
// host that cannot refuses rather than running it in a weaker tier.
func TestContainmentGateRefusesWorkTheSandboxCannotContain(t *testing.T) {
	cases := []struct {
		name  string
		level sandbox.Containment
		trust sandbox.Trust
		admit bool
	}{
		{"trusted work in a process jail", sandbox.ContainmentNone, sandbox.TrustTrusted, true},
		{"model-authored work in a process jail", sandbox.ContainmentNone, sandbox.TrustSemi, false},
		{"model-authored work in a kernel-confined tier", sandbox.ContainmentKernel, sandbox.TrustSemi, true},
		{"untrusted work in a container", sandbox.ContainmentContainer, sandbox.TrustUntrusted, false},
		{"untrusted work in a microvm", sandbox.ContainmentMicroVM, sandbox.TrustUntrusted, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := capability.NewContainmentGate(tier{level: tc.level})
			d := dispatch.New(dispatch.WithHook(gate))

			ran := false
			err := d.Govern(context.Background(), dispatch.Action{Name: "bash", Trust: tc.trust},
				func(context.Context) (dispatch.Metering, error) {
					ran = true
					return dispatch.Metering{}, nil
				})

			switch {
			case tc.admit && err != nil:
				t.Fatalf("%s was refused: %v", tc.name, err)
			case tc.admit && !ran:
				t.Fatal("an admitted action did not run")
			case !tc.admit && ran:
				t.Fatal("work ran in a tier that cannot contain it")
			case !tc.admit:
				if got := fault.Classify(err); got != fault.Forbidden {
					t.Fatalf("refusal class = %v, want Forbidden (a stronger tier, not a retry)", got)
				}
				if !errors.Is(err, sandbox.ErrInsufficientContainment) {
					t.Fatalf("refusal %v does not name the containment gap", err)
				}
			}
		})
	}
}

// TestContainmentGateWithoutASandboxIsPermissive: no containment context is wired,
// so the gate matches the rest of the waist's default-open-until-configured posture
// rather than blocking every action in a standalone run.
func TestContainmentGateWithoutASandboxIsPermissive(t *testing.T) {
	gate := capability.NewContainmentGate(nil)

	for _, trust := range []sandbox.Trust{sandbox.TrustTrusted, sandbox.TrustSemi, sandbox.TrustUntrusted} {
		if err := gate.Before(context.Background(), dispatch.Action{Name: "bash", Trust: trust}); err != nil {
			t.Errorf("an unwired gate refused %s work: %v", trust, err)
		}
	}

	// After holds no resource to release, so it settles any outcome without touching
	// the decision Before already made.
	gate.After(context.Background(), dispatch.Action{Name: "bash"}, dispatch.Metering{}, errors.New("work failed"))
	if err := gate.Before(context.Background(), dispatch.Action{Name: "bash"}); err != nil {
		t.Errorf("the gate stopped admitting after a failed action: %v", err)
	}
}

// TestPrincipalRoundTrip: the principal is who a run acts as, bound once at the top
// of a run alongside the grant. An unbound context is the standalone agent itself.
func TestPrincipalRoundTrip(t *testing.T) {
	if got := capability.PrincipalFromContext(context.Background()); got != "" {
		t.Errorf("a bare context carries principal %q, want the empty (standalone) principal", got)
	}

	ctx := capability.WithPrincipal(context.Background(), "run-1")
	if got := capability.PrincipalFromContext(ctx); got != "run-1" {
		t.Errorf("principal = %q, want run-1", got)
	}
	// The principal is independent of the grant: binding one leaves the other alone.
	ctx = capability.Into(ctx, capability.NewGrant("read"))
	if got := capability.PrincipalFromContext(ctx); got != "run-1" {
		t.Errorf("binding a grant changed the principal to %q", got)
	}
	// A nested bind narrows to the inner principal, so a child run acts as itself.
	if got := capability.PrincipalFromContext(capability.WithPrincipal(ctx, "run-2")); got != "run-2" {
		t.Errorf("principal = %q, want the innermost binding run-2", got)
	}
}
