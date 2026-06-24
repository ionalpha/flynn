package capability_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Property: with a grant bound, the admitter admits an action if and only if the
// grant lists it; a name outside the set is denied with Forbidden. This is the
// least-privilege contract the whole governance layer rests on.
func TestProp_AdmitterMatchesGrant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nameGen := rapid.StringMatching(`[a-z][a-z0-9_.]{0,8}`)
		allowed := rapid.SliceOfDistinct(nameGen, func(s string) string { return s }).Draw(rt, "allowed")
		query := nameGen.Draw(rt, "query")

		grant := capability.NewGrant(allowed...)
		ctx := capability.Into(context.Background(), grant)
		err := (capability.Admitter{}).Admit(ctx, dispatch.Action{Name: query})

		inGrant := false
		for _, a := range allowed {
			if a == query {
				inGrant = true
				break
			}
		}
		switch {
		case inGrant && err != nil:
			rt.Fatalf("granted %q denied: %v", query, err)
		case !inGrant && err == nil:
			rt.Fatalf("ungranted %q admitted", query)
		case !inGrant && fault.Classify(err) != fault.Forbidden:
			rt.Fatalf("denial of %q classified %v, want Forbidden", query, fault.Classify(err))
		}
		// Grant.Allows and the admitter must never disagree.
		if grant.Allows(query) != (err == nil) {
			rt.Fatalf("Allows(%q)=%v disagrees with admit err=%v", query, grant.Allows(query), err)
		}
	})
}

// Property: AllowAll and an absent grant both admit every action, while a
// deny-all grant (empty set) denies every action. The three permissiveness modes
// are total and never depend on the action name.
func TestProp_PermissivenessModes(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.StringMatching(`[a-z][a-z0-9_.]{0,8}`).Draw(rt, "name")
		a := dispatch.Action{Name: name}
		admit := capability.Admitter{}

		if err := admit.Admit(capability.Into(context.Background(), capability.AllowAll()), a); err != nil {
			rt.Fatalf("AllowAll denied %q: %v", name, err)
		}
		if err := admit.Admit(context.Background(), a); err != nil {
			rt.Fatalf("no-grant denied %q: %v", name, err)
		}
		if err := admit.Admit(capability.Into(context.Background(), capability.NewGrant()), a); err == nil {
			rt.Fatalf("deny-all admitted %q", name)
		}
	})
}
