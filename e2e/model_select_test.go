package e2e

import "testing"

// TestPersistedModelDefaultUsedWithoutFlag proves the headline behavior end to end: once a
// default model is recorded, a later run with no --model flag uses it. `flynn models use`
// records a hosted default; a goal run with the -model flag omitted then drives that model
// (the scripted server stands in for the recorded provider), so a user need not repeat
// --model.
func TestPersistedModelDefaultUsedWithoutFlag(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("used the saved default"))
	in := newInstance(t).withModel(fake)

	use := in.run("models", "use", "openai:gpt-e2e")
	requireExit(t, use, 0, "models use")

	// Drop the -model flag entirely; the run must fall back to the saved default.
	in.model = ""
	res := in.run("-no-learn", "goal", "use whatever model is the default")
	requireExit(t, res, 0, "goal with no -model flag")
	requireContains(t, res.combined(), "used the saved default", "the persisted default drove the run")
}

// TestModelsUseCLISurface covers the command-line surface: `flynn models use` records a
// hosted provider spec, `flynn models` shows it as the default, and an unknown id is
// refused.
func TestModelsUseCLISurface(t *testing.T) {
	in := newInstance(t)

	requireExit(t, in.run("models", "use", "openai:gpt-5.5"), 0, "models use hosted spec")
	requireContains(t, in.run("models").stdout, "openai:gpt-5.5", "models shows the default")

	if bad := in.run("models", "use", "notaprovider:x"); bad.code == 0 {
		t.Fatalf("models use of an unknown id should fail:\n%s", bad.combined())
	}
}
