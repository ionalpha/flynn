package capability_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

func TestGrantAllows(t *testing.T) {
	g := capability.NewGrant("read", "write", "", "read") // empties and dups ignored
	for _, a := range []string{"read", "write"} {
		if !g.Allows(a) {
			t.Fatalf("grant should allow %q", a)
		}
	}
	if g.Allows("bash") {
		t.Fatal("grant should not allow an unlisted action")
	}
	if g.Unrestricted() {
		t.Fatal("a listed grant is not unrestricted")
	}
	if got := g.Actions(); !reflect.DeepEqual(got, []string{"read", "write"}) {
		t.Fatalf("Actions() = %v, want [read write] (sorted, deduped)", got)
	}
}

func TestZeroGrantDeniesEverything(t *testing.T) {
	var g capability.Grant // zero value
	if g.Allows("read") || g.Unrestricted() {
		t.Fatal("the zero grant must deny everything")
	}
	if got := capability.NewGrant().Actions(); len(got) != 0 {
		t.Fatalf("empty grant Actions() = %v, want none", got)
	}
}

func TestAllowAllGrant(t *testing.T) {
	g := capability.AllowAll()
	if !g.Allows("anything") || !g.Allows("read") || !g.Unrestricted() {
		t.Fatal("AllowAll must admit every action")
	}
	if got := g.Actions(); len(got) != 0 {
		t.Fatalf("AllowAll Actions() = %v, want none (no enumerated set)", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := capability.FromContext(context.Background()); ok {
		t.Fatal("a bare context must carry no grant")
	}
	g := capability.NewGrant("read")
	got, ok := capability.FromContext(capability.Into(context.Background(), g))
	if !ok || !got.Allows("read") || got.Allows("write") {
		t.Fatalf("round-tripped grant = %+v (ok=%v)", got, ok)
	}
}

func TestAdmitterPermissiveWithoutGrant(t *testing.T) {
	// No grant bound: the standalone zero-config posture admits everything.
	if err := (capability.Admitter{}).Admit(context.Background(), dispatch.Action{Name: "bash"}); err != nil {
		t.Fatalf("admitter with no grant must admit, got %v", err)
	}
}

func TestAdmitterEnforcesBoundGrant(t *testing.T) {
	ctx := capability.Into(context.Background(), capability.NewGrant("read", "write"))
	a := capability.Admitter{}

	if err := a.Admit(ctx, dispatch.Action{Name: "read"}); err != nil {
		t.Fatalf("granted action denied: %v", err)
	}
	err := a.Admit(ctx, dispatch.Action{Name: "bash"})
	if err == nil {
		t.Fatal("ungranted action must be denied")
	}
	if fault.Classify(err) != fault.Forbidden {
		t.Fatalf("denial class = %v, want Forbidden", fault.Classify(err))
	}
	// Forbidden is terminal-by-reaction: it must never be confused with a retryable
	// transient failure.
	if errors.Is(err, context.Canceled) {
		t.Fatal("unexpected wrapping")
	}
}

func TestAdmitterAllowAllAdmitsEverything(t *testing.T) {
	ctx := capability.Into(context.Background(), capability.AllowAll())
	if err := (capability.Admitter{}).Admit(ctx, dispatch.Action{Name: "anything"}); err != nil {
		t.Fatalf("AllowAll grant denied an action: %v", err)
	}
}

func TestAdmitterDenyAllGrantDeniesEverything(t *testing.T) {
	// A bound but empty grant is an explicit deny-all policy.
	ctx := capability.Into(context.Background(), capability.NewGrant())
	if err := (capability.Admitter{}).Admit(ctx, dispatch.Action{Name: "read"}); fault.Classify(err) != fault.Forbidden {
		t.Fatalf("deny-all grant should Forbid, got %v", err)
	}
}
