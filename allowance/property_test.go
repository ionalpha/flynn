package allowance_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Property: an action runs only where the run holds a declaration that names it, and either
// names the same target or names none. Nothing else about the draw can change that answer,
// which is the guarantee the gate exists to make: the authority for an irreversible action
// is what was written down, and nothing about what the run is doing widens it.
//
// The property is checked against the rule stated in words rather than against Covers, so a
// change to how a declaration is matched has to defend itself here. It is drawn over a small
// alphabet on purpose: the interesting cases are collisions (the same action declared under
// a different target, a target declared for a different action), and a wide alphabet would
// make those rare.
func TestProp_OnlyADeclarationLetsAMarkedActionRun(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SampledFrom([]string{"deploy", "secret.release", "fs.delete", "read_file"})
		targets := rapid.SampledFrom([]string{"", "prod", "staging"})

		marked := rapid.SliceOfN(names, 0, 4).Draw(rt, "marked")
		var decls []allowance.Declaration
		for i, name := range rapid.SliceOfN(names, 0, 4).Draw(rt, "declared") {
			decls = append(decls, allowance.Declaration{
				Action: name,
				Target: targets.Draw(rt, "declaredTarget"+string(rune('a'+i))),
			})
		}

		action := names.Draw(rt, "action")
		target := targets.Draw(rt, "target")

		ctx := allowance.Into(context.Background(), decls...)
		if target != "" {
			ctx = allowance.WithTarget(ctx, target)
		}
		err := allowance.NewGate(allowance.NewActions(marked...)).
			Before(ctx, dispatch.Action{Name: action})

		isMarked := false
		for _, m := range marked {
			if m == action {
				isMarked = true
			}
		}
		declared := false
		for _, d := range decls {
			if d.Action == action && (d.Target == "" || d.Target == target) {
				declared = true
			}
		}

		if want := isMarked && !declared; (err != nil) != want {
			rt.Fatalf("refused = %v, want %v (action %q target %q marked %v declared %+v)",
				err != nil, want, action, target, marked, decls)
		}
		if err != nil && fault.CodeOf(err) != allowance.CodeRequired {
			rt.Fatalf("code = %q, want %q: the reconciler finds the ask by this code alone",
				fault.CodeOf(err), allowance.CodeRequired)
		}
	})
}

// Property: binding declarations in any number of steps is the same as binding them at once.
// A fan-out's child assembles its authority from what it inherited and what its host adds,
// and an authority that depended on which order those arrived in would be one nobody could
// state.
func TestProp_BindingOrderDoesNotChangeAuthority(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SampledFrom([]string{"deploy", "secret.release", "fs.delete"})
		decls := make([]allowance.Declaration, 0, 4)
		for _, name := range rapid.SliceOfN(names, 0, 4).Draw(rt, "declared") {
			decls = append(decls, allowance.Declaration{Action: name})
		}
		split := rapid.IntRange(0, len(decls)).Draw(rt, "split")

		atOnce := allowance.Into(context.Background(), decls...)
		inSteps := allowance.Into(allowance.Into(context.Background(), decls[:split]...), decls[split:]...)

		for _, name := range []string{"deploy", "secret.release", "fs.delete", "read_file"} {
			a, b := allowance.FromContext(atOnce), allowance.FromContext(inSteps)
			if allowance.Covers(a, name, "") != allowance.Covers(b, name, "") {
				rt.Fatalf("%q is covered by one binding order and not the other: %+v vs %+v", name, a, b)
			}
		}
	})
}
