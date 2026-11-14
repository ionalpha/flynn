package allowance_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// TestFailClosedDeclaration proves a declaration authorizes only what it names. The zero
// Declaration, an empty action, and a name that differs by anything but surrounding
// space authorize nothing. An action nobody marked, and a nil policy, need no declaration
// by design; that is pinned so changing it is a decision.
func TestFailClosedDeclaration(t *testing.T) {
	t.Run("the zero declaration authorizes nothing", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			action := rapid.String().Draw(rt, "action")
			target := rapid.String().Draw(rt, "target")
			if allowance.Covers([]allowance.Declaration{{}}, action, target) {
				rt.Fatalf("a zero declaration covers %q on %q", action, target)
			}
		})
	})
	t.Run("an empty action is never covered", func(t *testing.T) {
		decls := []allowance.Declaration{{Action: ""}, {Action: " "}, {Action: "deploy"}}
		for _, action := range []string{"", " ", "\t"} {
			if allowance.Covers(decls, action, "") {
				t.Errorf("the action %q was covered", action)
			}
		}
	})
	t.Run("no declarations cover nothing", func(t *testing.T) {
		if allowance.Covers(nil, "deploy", "") {
			t.Fatal("a nil declaration list covered an action")
		}
	})
	t.Run("a narrowed declaration does not cover another or an unbound target", func(t *testing.T) {
		decls := []allowance.Declaration{{Action: "deploy", Target: "prod"}}
		for _, target := range []string{"", "staging", "prod/eu", "PROD"} {
			if allowance.Covers(decls, "deploy", target) {
				t.Errorf("a declaration for prod covered target %q", target)
			}
		}
		if allowance.Covers(decls, "deploy-all", "prod") {
			t.Error("a declaration covered a different action")
		}
	})
}

// TestFailClosedAllowanceGate proves an action the policy marks is refused unless a
// declaration covers it, whatever the context carries.
func TestFailClosedAllowanceGate(t *testing.T) {
	deploy := dispatch.Action{Name: "deploy"}
	gov := func(p allowance.Policy) *dispatch.Dispatcher {
		return dispatch.New(dispatch.WithHook(allowance.NewGate(p)))
	}
	run := func(d *dispatch.Dispatcher, ctx context.Context, a dispatch.Action) (bool, error) {
		ran := false
		err := d.Govern(ctx, a, func(context.Context) (dispatch.Metering, error) { ran = true; return dispatch.Metering{}, nil })
		return ran, err
	}

	t.Run("a marked action with no declarations needs approval", func(t *testing.T) {
		ran, err := run(gov(allowance.NewActions("deploy")), context.Background(), deploy)
		if ran || fault.Classify(err) != fault.NeedsApproval {
			t.Fatalf("ran=%v err=%v, want needs-approval", ran, err)
		}
	})
	t.Run("a zero declaration on the context does not authorize", func(t *testing.T) {
		ctx := allowance.Into(context.Background(), allowance.Declaration{})
		ran, err := run(gov(allowance.NewActions("deploy")), ctx, deploy)
		if ran || err == nil {
			t.Fatalf("ran=%v err=%v, want a refusal", ran, err)
		}
	})
	t.Run("an action named the empty string is refused when marked", func(t *testing.T) {
		ctx := allowance.Into(context.Background(), allowance.Declaration{})
		ran, err := run(gov(allowance.Actions{"": true}), ctx, dispatch.Action{})
		if ran || err == nil {
			t.Fatalf("ran=%v err=%v, want a refusal", ran, err)
		}
	})
	t.Run("blank names do not enrol an action", func(t *testing.T) {
		if len(allowance.NewActions("", " ", "\t")) != 0 {
			t.Fatal("a blank name was enrolled")
		}
	})
	t.Run("an unlisted action and a nil policy need no declaration by design", func(t *testing.T) {
		for name, p := range map[string]allowance.Policy{
			"unlisted": allowance.NewActions("other"),
			"nil":      nil,
			"nil set":  allowance.Actions(nil),
		} {
			if ran, err := run(gov(p), context.Background(), deploy); !ran || err != nil {
				t.Errorf("%s policy: ran=%v err=%v; an unmarked action is admitted by design", name, ran, err)
			}
		}
	})
}
