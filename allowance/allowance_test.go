package allowance_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

func act(name string) dispatch.Action { return dispatch.Action{Name: name} }

// TestUnmarkedActionRunsUndeclared is the default and every run that predates the gate: an
// action the host has not marked needs nothing, so wiring the gate changes no behaviour on
// its own.
func TestUnmarkedActionRunsUndeclared(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy"))
	if err := g.Before(context.Background(), act("write_file")); err != nil {
		t.Fatalf("an unmarked action was refused: %v", err)
	}
}

// TestMarkedActionNeedsADeclaration is the whole gate. The run holds no declaration, so the
// action does not happen, whatever the objective implied about wanting it.
func TestMarkedActionNeedsADeclaration(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy"))
	err := g.Before(context.Background(), act("deploy"))
	if err == nil {
		t.Fatal("an undeclared irreversible action was admitted")
	}
	if got := fault.Classify(err); got != fault.NeedsApproval {
		t.Errorf("class = %q, want %q: nobody has said the run may do this, which is a "+
			"question with an answer, not a rule against it", got, fault.NeedsApproval)
	}
	if got := fault.CodeOf(err); got != allowance.CodeRequired {
		t.Errorf("code = %q, want %q: the reconciler reads the record back for exactly this "+
			"code to turn the refusal into an ask", got, allowance.CodeRequired)
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("message %q does not name the action", err)
	}
}

// TestDeclaredActionRuns is the run whose author wrote the authority down before it started.
func TestDeclaredActionRuns(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy"))
	ctx := allowance.Into(context.Background(), allowance.Declaration{Action: "deploy"})
	if err := g.Before(ctx, act("deploy")); err != nil {
		t.Fatalf("a declared action was refused: %v", err)
	}
}

// TestADeclarationDoesNotSpreadToAnotherAction is the substitution an authorization must
// never cover: declaring one irreversible action is not declaring the next one.
func TestADeclarationDoesNotSpreadToAnotherAction(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy", "secret.release"))
	ctx := allowance.Into(context.Background(), allowance.Declaration{Action: "deploy"})
	if err := g.Before(ctx, act("secret.release")); err == nil {
		t.Fatal("a declaration for one action authorized another")
	}
}

// TestATargetedDeclarationCoversOnlyThatTarget is the narrow form doing its job: the
// authority was for one target and the second target is a different decision.
func TestATargetedDeclarationCoversOnlyThatTarget(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy"))
	ctx := allowance.Into(context.Background(), allowance.Declaration{Action: "deploy", Target: "staging"})

	if err := g.Before(allowance.WithTarget(ctx, "staging"), act("deploy")); err != nil {
		t.Fatalf("the declared target was refused: %v", err)
	}
	err := g.Before(allowance.WithTarget(ctx, "prod"), act("deploy"))
	if err == nil {
		t.Fatal("a declaration for staging authorized prod")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("message %q does not name the target that was refused, which is the only "+
			"part that tells the author what to declare", err)
	}
}

// TestAnUntargetedDeclarationCoversEveryTarget is the widest form, and it is explicit: an
// author who names no target has authorized the action wherever it lands.
func TestAnUntargetedDeclarationCoversEveryTarget(t *testing.T) {
	g := allowance.NewGate(allowance.NewActions("deploy"))
	ctx := allowance.Into(context.Background(), allowance.Declaration{Action: "deploy"})
	if err := g.Before(allowance.WithTarget(ctx, "prod"), act("deploy")); err != nil {
		t.Fatalf("an untargeted declaration did not cover a target: %v", err)
	}
}

// TestCoversDoesNotInterpretATargetAsAPrefix pins the comparison. A rule where one declared
// string covers a family of undeclared ones is where an authorization quietly widens, and a
// path-shaped target is exactly where that would be tempting.
func TestCoversDoesNotInterpretATargetAsAPrefix(t *testing.T) {
	decls := []allowance.Declaration{{Action: "fs.delete", Target: "/home/u/.config"}}
	if allowance.Covers(decls, "fs.delete", "/home/u/.config/app/settings") {
		t.Error("a declared target covered something beneath it")
	}
	if allowance.Covers(decls, "fs.delete", "/home/u/.config-backup") {
		t.Error("a declared target covered a name it is a prefix of")
	}
	if !allowance.Covers(decls, "fs.delete", "/home/u/.config") {
		t.Error("a declared target did not cover itself")
	}
}

// TestCoversRefusesABlankAction is the degenerate call: nothing authorizes an action with
// no name, and a declaration with no action authorizes nothing.
func TestCoversRefusesABlankAction(t *testing.T) {
	if allowance.Covers([]allowance.Declaration{{Action: "deploy"}}, "  ", "") {
		t.Error("a blank action was covered")
	}
	if allowance.Covers([]allowance.Declaration{{Action: "  "}}, "deploy", "") {
		t.Error("a declaration naming no action covered one")
	}
}

// TestDeclarationsAccumulate is how a run assembles its authority from more than one bind,
// which is what a fan-out's child does when it inherits and its host adds nothing.
func TestDeclarationsAccumulate(t *testing.T) {
	ctx := allowance.Into(context.Background(), allowance.Declaration{Action: "deploy"})
	ctx = allowance.Into(ctx, allowance.Declaration{Action: "secret.release"})
	if got := len(allowance.FromContext(ctx)); got != 2 {
		t.Fatalf("declarations = %d, want 2", got)
	}
	g := allowance.NewGate(allowance.NewActions("deploy", "secret.release"))
	for _, name := range []string{"deploy", "secret.release"} {
		if err := g.Before(ctx, act(name)); err != nil {
			t.Errorf("%s was refused after both were declared: %v", name, err)
		}
	}
}

// TestNoDeclarationsBoundReadsAsNone is the plain context every standalone call has.
func TestNoDeclarationsBoundReadsAsNone(t *testing.T) {
	if got := allowance.FromContext(context.Background()); got != nil {
		t.Errorf("declarations = %v, want none on an unbound context", got)
	}
}

// TestNewActionsDropsBlanks keeps a stray empty flag value from enrolling an action named
// the empty string, which nothing dispatches and which would read as a marked action.
func TestNewActionsDropsBlanks(t *testing.T) {
	set := allowance.NewActions("deploy", "  ", "")
	if len(set) != 1 || !set["deploy"] {
		t.Fatalf("actions = %v, want just deploy", set)
	}
	if set.Outside(act("")) {
		t.Error("the empty action name was marked")
	}
}

// TestNilPolicyMarksNothing is the zero-config default: a gate built over no policy admits
// everything rather than refusing everything.
func TestNilPolicyMarksNothing(t *testing.T) {
	if err := allowance.NewGate(nil).Before(context.Background(), act("deploy")); err != nil {
		t.Fatalf("a gate with no policy refused an action: %v", err)
	}
}

// TestAfterIsANoOp holds the hook contract: the decision is made before the action runs, so
// there is nothing to do on the way out and nothing that could change the outcome late.
func TestAfterIsANoOp(t *testing.T) {
	allowance.NewGate(allowance.NewActions("deploy")).After(
		context.Background(), act("deploy"), dispatch.Metering{}, nil)
}
