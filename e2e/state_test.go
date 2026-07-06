package e2e

import (
	"strings"
	"testing"
)

// TestStatePersistsAcrossInvocations asserts durable state survives across separate
// binary invocations against one data dir: two independent goals each persist their run,
// both are listed together afterward, and each verifies on its own. A second copy of the
// truth drifting, or a run clobbering the prior one, would fail this.
func TestStatePersistsAcrossInvocations(t *testing.T) {
	fake := newFakeOpenAIFunc(t, func(req oaiRequest, _ int) oaiReply {
		// Answer from the objective so each run's record carries its own text.
		if containsObjective(req, "alpha") {
			return finalText("alpha done")
		}
		return finalText("beta done")
	})
	in := newInstance(t).withModel(fake)

	first := in.run("-no-learn", "goal", "run alpha")
	requireExit(t, first, 0, "first goal")
	firstID := in.runID(first)

	second := in.run("-no-learn", "goal", "run beta")
	requireExit(t, second, 0, "second goal")
	secondID := in.runID(second)

	if firstID == secondID {
		t.Fatalf("two goals shared a run id %s; runs were not independent", firstID)
	}

	// A third invocation sees both runs; the store persisted across all three processes.
	runs := in.run("runs")
	requireExit(t, runs, 0, "runs")
	requireContains(t, runs.stdout, firstID, "first run persisted")
	requireContains(t, runs.stdout, secondID, "second run persisted")
	requireContains(t, runs.stdout, "run alpha", "first objective persisted")
	requireContains(t, runs.stdout, "run beta", "second objective persisted")

	// Each run is independently verifiable from the durable store.
	requireExit(t, in.verify(firstID), 0, "verify first run")
	requireExit(t, in.verify(secondID), 0, "verify second run")
}

// containsObjective reports whether any user message in the request carries sub, used to
// route the scripted reply by which goal is running.
func containsObjective(req oaiRequest, sub string) bool {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}
