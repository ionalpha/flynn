package goal

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/allowance"
)

// TestDeclarationsCarryTheTargetToTheWaist keeps the spec's form and the gate's form in
// step: a narrowing written on the goal has to reach the gate that enforces it.
func TestDeclarationsCarryTheTargetToTheWaist(t *testing.T) {
	got := Declarations([]Allowance{{Action: "deploy", Target: "staging"}})
	want := []allowance.Declaration{{Action: "deploy", Target: "staging"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("declarations = %+v, want %+v", got, want)
	}
	if Declarations(nil) != nil {
		t.Error("a goal declaring nothing produced declarations")
	}
}

// TestAllowanceCoversIsAskedAboutTheActionOnly is the coverage question the record can
// answer, and the test says so: a refusal names the action and not what it was attempted
// against, so a targeted declaration answers the ask about that action.
func TestAllowanceCoversIsAskedAboutTheActionOnly(t *testing.T) {
	alls := []Allowance{{Action: "deploy", Target: "staging"}}
	if !AllowanceCovers(alls, "deploy") {
		t.Error("a declaration of the action did not cover it")
	}
	if AllowanceCovers(alls, "secret.release") {
		t.Error("a declaration of one action covered another")
	}
	if AllowanceCovers(alls, " ") {
		t.Error("a blank action was covered")
	}
}

func allowanceRefused(actions ...string) []Refusal {
	out := make([]Refusal, 0, len(actions))
	for _, a := range actions {
		out = append(out, Refusal{Rule: allowance.CodeRequired, Action: a})
	}
	return out
}

// TestReadAllowanceAskNamesWhatStoppedTheRunFirst is the ask a person is handed. It names
// the action the run reached first rather than whichever sorts earliest, because the first
// one is where the run stopped being the run its author asked for.
func TestReadAllowanceAskNamesWhatStoppedTheRunFirst(t *testing.T) {
	ask, need := ReadAllowanceAsk(allowanceRefused("secret.release", "deploy"), nil)
	if !need {
		t.Fatal("a run refused an undeclared irreversible action produced no ask")
	}
	if ask.Action != "secret.release" {
		t.Errorf("ask = %q, want secret.release", ask.Action)
	}
	if ask.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", ask.Attempts)
	}
}

// TestReadAllowanceAskCountsTheAttempts distinguishes a run that met the boundary once from
// one that kept trying, which is what tells the person answering whether the objective can
// be met any other way.
func TestReadAllowanceAskCountsTheAttempts(t *testing.T) {
	ask, need := ReadAllowanceAsk(allowanceRefused("deploy", "deploy", "deploy"), nil)
	if !need || ask.Attempts != 3 {
		t.Fatalf("ask = %+v, want deploy refused 3 times", ask)
	}
	if !strings.Contains(ask.AskReason(), "3 times") {
		t.Errorf("reason %q does not say how many times", ask.AskReason())
	}
	single, _ := ReadAllowanceAsk(allowanceRefused("deploy"), nil)
	if strings.Contains(single.AskReason(), "times") {
		t.Errorf("reason %q counts a single attempt", single.AskReason())
	}
}

// TestADeclaredActionIsNoLongerAnAsk is what lets a paused goal resume. The refusal stays on
// the record, because it happened; what changes is that the question it raised has been
// answered, and the record does not have to be edited for the run to carry on.
func TestADeclaredActionIsNoLongerAnAsk(t *testing.T) {
	record := allowanceRefused("deploy")
	if _, need := ReadAllowanceAsk(record, []Allowance{{Action: "deploy"}}); need {
		t.Fatal("a refusal the author has since declared still reads as an ask")
	}
	if _, need := ReadAllowanceAsk(record, []Allowance{{Action: "secret.release"}}); !need {
		t.Error("declaring a different action answered the ask")
	}
}

// TestOtherRefusalsAreNotAnAsk keeps the ask to the one refusal it is about. A capability
// denial is a run acting outside its grant, and the answer to it is not a declaration.
func TestOtherRefusalsAreNotAnAsk(t *testing.T) {
	record := refusals("capability_denied", "deploy", "containment_unavailable", "bash")
	if _, need := ReadAllowanceAsk(record, nil); need {
		t.Error("a refusal from another gate was read as an allowance ask")
	}
}

// TestARefusalNamingNoActionIsNotAnAsk refuses to invent the ask. A record that does not say
// what was refused cannot be turned into an instruction about what to declare.
func TestARefusalNamingNoActionIsNotAnAsk(t *testing.T) {
	if _, need := ReadAllowanceAsk([]Refusal{{Rule: allowance.CodeRequired}}, nil); need {
		t.Error("a refusal naming no action produced an ask")
	}
	if _, need := ReadAllowanceAsk(nil, nil); need {
		t.Error("an empty record produced an ask")
	}
}

// TestAskReasonSaysWhatAnsweringItMeans is written for the person holding the stopped goal:
// it names the action, and it says that the two ways out are declaring it and ending the
// run. A message that only reported the block would leave them to guess at both.
func TestAskReasonSaysWhatAnsweringItMeans(t *testing.T) {
	reason := AllowanceAsk{Action: "fs.delete", Attempts: 1}.AskReason()
	for _, want := range []string{"fs.delete", "declare an allowance", "the run is over"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not say %q", reason, want)
		}
	}
}
