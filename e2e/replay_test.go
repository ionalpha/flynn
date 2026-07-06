package e2e

import (
	"testing"
)

// TestReplayIsDeterministic asserts replay determinism through the binary: rendering a
// past run from its recorded events twice yields byte-identical output. A run is a
// projection of its event log, so replaying it must not depend on wall-clock, ordering,
// or per-process state; a difference would mean the record does not fully determine the
// rendered run.
func TestReplayIsDeterministic(t *testing.T) {
	fake := newFakeOpenAIQueue(
		t,
		toolCall("w", "write", `{"path":"note.txt","content":"recorded"}`),
		finalText("Wrote note.txt and finished."),
	)
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "produce a replayable run")
	requireExit(t, res, 0, "goal")
	runID := in.runID(res)

	first := in.run("inspect", runID)
	requireExit(t, first, 0, "inspect #1")
	second := in.run("inspect", runID)
	requireExit(t, second, 0, "inspect #2")

	if first.stdout != second.stdout {
		t.Fatalf("replay not deterministic; two renders of run %s differ:\n--- first ---\n%s\n--- second ---\n%s",
			runID, first.stdout, second.stdout)
	}
	// The replay reflects the recorded work: the tool the model called shows up.
	requireContains(t, first.combined(), "note.txt", "replay shows the recorded tool use")
}
