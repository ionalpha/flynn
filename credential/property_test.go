package credential

import (
	"context"
	"testing"

	"pgregory.net/rapid"
)

// TestPropSingleDefault asserts that across any sequence of Put operations to one
// integration, the store never holds more than one default credential, and that a
// default Put leaves exactly that credential as the default. This is the invariant
// callers rely on when resolving a bare integration reference to "the default".
func TestPropSingleDefault(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := newStore(t)
		ctx := context.Background()
		names := rapid.SampledFrom([]string{"a", "b", "c"})

		n := rapid.IntRange(1, 12).Draw(rt, "ops")
		lastDefault := ""
		for range n {
			name := names.Draw(rt, "name")
			isDefault := rapid.Bool().Draw(rt, "isDefault")
			if _, err := s.Put(ctx, Spec{Integration: "cf", Name: name, IsDefault: isDefault}); err != nil {
				rt.Fatalf("put: %v", err)
			}
			if isDefault {
				lastDefault = name
			} else if name == lastDefault {
				// Re-putting the current default without the flag clears it.
				lastDefault = ""
			}
		}

		creds, err := s.List(ctx, "cf")
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		defaults := make([]string, 0, 1)
		for _, c := range creds {
			if c.Spec.IsDefault {
				defaults = append(defaults, c.Spec.Name)
			}
		}
		if len(defaults) > 1 {
			rt.Fatalf("more than one default: %v", defaults)
		}
		if lastDefault == "" {
			if len(defaults) != 0 {
				rt.Fatalf("expected no default, got %v", defaults)
			}
		} else {
			if len(defaults) != 1 || defaults[0] != lastDefault {
				rt.Fatalf("expected default %q, got %v", lastDefault, defaults)
			}
		}
	})
}

// TestPropRoleOrdering asserts Permits is consistent with the rank order over the
// valid roles: a role permits exactly the actions at or below its rank, and is
// reflexive.
func TestPropRoleOrdering(t *testing.T) {
	roles := []Role{RoleRead, RoleOperator, RoleAdmin}
	rapid.Check(t, func(rt *rapid.T) {
		cred := rapid.SampledFrom(roles).Draw(rt, "cred")
		required := rapid.SampledFrom(roles).Draw(rt, "required")
		want := cred.rank() >= required.rank()
		if got := cred.Permits(required); got != want {
			rt.Fatalf("Permits(%q,%q)=%v want %v", cred, required, got, want)
		}
		if !cred.Permits(cred) {
			rt.Fatalf("role %q should permit itself", cred)
		}
	})
}
