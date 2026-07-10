package review_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/archetypes/review"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/tools/github"
)

// admit runs one action through a dispatch waist configured exactly as a run's is:
// the capability admitter, consulting whatever grant is bound to the context. It
// reports the error the waist raised, or nil when the action was allowed to run.
func admit(ctx context.Context, name string) error {
	d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}))
	return d.Govern(ctx, dispatch.Action{Name: name},
		func(context.Context) (dispatch.Metering, error) { return dispatch.Metering{}, nil })
}

// bound returns a context carrying the reviewer's grant, which is what a run does
// before it takes its first action.
func bound() context.Context { return capability.Into(context.Background(), review.Grant()) }

// --- the refusal ------------------------------------------------------------

// A reviewer reads a diff and writes to an API. It executes nothing. These actions
// are absent from its grant, so the waist refuses them before any side effect: not
// because the model was asked not to, and not because the toolset omits them, but
// because the authority to take them was never granted.
func TestUngrantedActionsAreRefusedAtTheWaist(t *testing.T) {
	ctx := bound()
	for _, name := range []string{
		"bash",              // run a command
		"read",              // read the working tree
		"write",             // write a file
		"edit",              // edit a file
		"glob",              // enumerate the working tree
		"grep",              // search the working tree
		mission.ActionSpawn, // delegate to a child run
		"learn.distill",     // write to memory
		"github_merge",      // an action no tool implements
	} {
		t.Run(name, func(t *testing.T) {
			err := admit(ctx, name)
			if err == nil {
				t.Fatalf("action %q was admitted; a reviewer must not hold it", name)
			}
			if got := fault.Classify(err); got != fault.Forbidden {
				t.Fatalf("action %q refused with class %v, want Forbidden", name, got)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("refusal does not name the action: %v", err)
			}
		})
	}
}

// The other half of the same claim: the actions a reviewer needs are admitted, so
// the grant is narrow rather than merely broken. A grant that refused everything
// would pass the test above and review nothing.
func TestGrantedActionsAreAdmitted(t *testing.T) {
	ctx := bound()
	for _, name := range review.Capabilities() {
		t.Run(name, func(t *testing.T) {
			if err := admit(ctx, name); err != nil {
				t.Fatalf("action %q was refused; a reviewer needs it: %v", name, err)
			}
		})
	}
}

// TestProp_OnlyGrantedActionsAreAdmitted is the general form: for any action name at
// all, the waist admits it exactly when the reviewer's grant names it. There is no
// input, however odd, that widens a reviewer's authority.
func TestProp_OnlyGrantedActionsAreAdmitted(t *testing.T) {
	granted := review.Capabilities()
	ctx := bound()

	// Draw from the granted names as often as from arbitrary ones, so the property
	// exercises both branches. An arbitrary string alone would essentially never hit a
	// granted name, and the test would only ever prove that the waist refuses things.
	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.OneOf(
			rapid.SampledFrom(granted),
			rapid.String(),
			// Near-misses: a granted name with something appended, and a prefix of one.
			rapid.Map(rapid.SampledFrom(granted), func(s string) string { return s + "_" }),
			rapid.Map(rapid.SampledFrom(granted), func(s string) string { return s[:len(s)-1] }),
		).Draw(rt, "action")

		want := slices.Contains(granted, name)
		got := admit(ctx, name) == nil
		if got != want {
			rt.Fatalf("action %q: admitted = %v, granted = %v", name, got, want)
		}
	})
}

// The trap, asserted rather than assumed. Package capability treats a context with
// no grant as permissive: that is its zero-config default, and it means a reviewer
// assembled without binding its grant runs with unlimited authority while looking
// exactly like a reviewer from the outside. Whoever wires a run must call
// capability.Into. If this test ever fails, the default changed and the warning in
// the package doc should go.
func TestUnboundContextIsPermissiveSoTheGrantMustBeBound(t *testing.T) {
	if err := admit(context.Background(), "bash"); err != nil {
		t.Fatalf("an unbound context refused %q, so the permissive default changed: %v", "bash", err)
	}
	if err := admit(bound(), "bash"); err == nil {
		t.Fatal("binding the grant did not refuse bash")
	}
}

// --- the authority is the toolset -------------------------------------------

// Every tool a reviewer holds is granted, and every granted action other than the
// model call is a tool it holds. A capability with no tool is authority nobody can
// exercise; a tool with no capability is a tool the waist refuses. Either is a bug,
// and both are silent without this test.
func TestCapabilitiesMatchTheToolset(t *testing.T) {
	tools, err := review.Tools(testConfig(t))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	toolNames := make([]string, 0, len(tools))
	for _, tl := range tools {
		toolNames = append(toolNames, tl.Def().Name)
	}
	slices.Sort(toolNames)

	want := review.Capabilities()
	want = slices.DeleteFunc(want, func(s string) bool { return s == mission.ActionModelGenerate })

	if !slices.Equal(toolNames, want) {
		t.Fatalf("toolset %v does not match the grant %v", toolNames, want)
	}
}

// Capabilities returns a copy, so a caller cannot widen a reviewer's authority by
// writing through the slice it was handed.
func TestCapabilitiesCannotBeWidenedThroughTheReturnedSlice(t *testing.T) {
	got := review.Capabilities()
	got[0] = "bash"

	if slices.Contains(review.Capabilities(), "bash") {
		t.Fatal("mutating the returned slice widened the reviewer's capabilities")
	}
	if err := admit(bound(), "bash"); err == nil {
		t.Fatal("mutating the returned slice widened the grant")
	}
}

// --- the archetype ----------------------------------------------------------

// The archetype records what produced a review: the instruction, the authority, the
// loop, and the model. A verdict cites it, so these fields are the reviewer's
// identity rather than incidental configuration.
func TestArchetypePinsInstructionAuthorityAndLoop(t *testing.T) {
	spec := review.Archetype("anthropic:claude-sonnet-5")

	if spec.System != review.SystemPrompt {
		t.Error("archetype does not carry the reviewer's standing instruction")
	}
	if !slices.Equal(spec.Capabilities, review.Capabilities()) {
		t.Errorf("archetype capabilities %v do not match the grant %v", spec.Capabilities, review.Capabilities())
	}
	if spec.Driver != driver.NameDefault {
		t.Errorf("archetype driver = %q, want the general-purpose loop %q", spec.Driver, driver.NameDefault)
	}
	if spec.Model != "anthropic:claude-sonnet-5" {
		t.Errorf("archetype model = %q", spec.Model)
	}
	// The loop the archetype names must actually resolve, or a run fails at assembly.
	if _, err := driver.Default().Resolve(spec.Driver); err != nil {
		t.Fatalf("the archetype names a driver that does not resolve: %v", err)
	}
}

// An empty model defers to the host, so the same reviewer runs on a frontier model
// or a local one without a second archetype.
func TestArchetypeWithNoModelDefersToTheHost(t *testing.T) {
	if got := review.Archetype("").Model; got != "" {
		t.Fatalf("model = %q, want empty so the host's choice applies", got)
	}
}

// --- the instruction --------------------------------------------------------

// The prompt's whole job is to stop the model from posting a wall of observations.
// These are the load it carries; losing one is how a reviewer becomes unreadable.
func TestSystemPromptDemandsEvidenceAndForbidsNitpicks(t *testing.T) {
	p := review.SystemPrompt
	for _, required := range []string{
		"a file, a line, and a concrete scenario", // a finding must be evidenced
		"you do not have a finding",               // and refused otherwise
		"Style, naming, and formatting",           // nitpicks are named and excluded
		"never opens a second conversation",       // re-posting reconciles, it does not duplicate
		"read the whole change",                   // approval is a claim about coverage
	} {
		if !strings.Contains(p, required) {
			t.Errorf("the reviewer's instruction lost its rule about %q", required)
		}
	}
	if strings.Contains(p, "nitpick") {
		t.Error("the instruction says 'nitpick' rather than naming what it means")
	}
}

// testConfig is a Config complete enough to build a toolset. It reaches no network:
// nothing here calls the API.
func testConfig(t *testing.T) github.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return github.Config{
		App:   github.App{Issuer: "Iv1.test", InstallationID: 1, PrivateKey: key},
		Owner: "ionalpha",
		Repo:  "flynn",
	}
}

// TestSystemPromptStatesTheRetractionContract pins the sentence the resolve rule
// depends on. The reviewer retracts a finding it does not post again, so a prompt that
// told it not to repeat itself would have it silently withdraw live objections. The
// rule reads a silence; the prompt is what gives that silence a meaning.
func TestSystemPromptStatesTheRetractionContract(t *testing.T) {
	for _, want := range []string{
		"Post every finding that still stands",
		"a finding you do not post is a finding you no longer make",
		"retracts it",
	} {
		if !strings.Contains(review.SystemPrompt, want) {
			t.Errorf("the standing instruction does not say %q, so a silence means nothing", want)
		}
	}
	if strings.Contains(review.SystemPrompt, "Do not repeat a finding you have already posted") {
		t.Error("the instruction still tells the reviewer not to repeat itself, which retracts live findings")
	}
}
