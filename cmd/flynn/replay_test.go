package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/session"
)

// TestRunRecordStateReflectsSeal proves the picker's per-run record state reads
// recording before a run is sealed and sealed after, so the resume menu shows which
// runs carry a durable signed record.
func TestRunRecordStateReflectsSeal(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	if got := runRecordState(ctx, store, runID); got != session.RecordRecording {
		t.Errorf("record state before seal = %q, want recording", got)
	}
	if err := sealRunFromStore(ctx, store, runID, selfCertifyingSigner(t)); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got := runRecordState(ctx, store, runID); got != session.RecordSealed {
		t.Errorf("record state after seal = %q, want sealed", got)
	}
}

// TestShellReplayReRendersRun drives a turn, then /replay, and proves the recorded run
// is re-rendered into the scrollback between clear delimiters, so the user can see the
// run as it was recorded on demand.
func TestShellReplayReRendersRun(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "the answer is 42"})

	host.submit("what is the answer", nil)
	waitIdle(t, host)

	host.submit("/replay", nil)
	waitIdle(t, host)

	got := ui.transcript()
	for _, want := range []string{"replay of run", "end of replay", "the answer is 42"} {
		if !strings.Contains(got, want) {
			t.Errorf("replay output missing %q in:\n%s", want, got)
		}
	}
}

// TestShellReplayEmptyRun proves /replay before any turn reports there is nothing to
// replay rather than rendering an empty span.
func TestShellReplayEmptyRun(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "unused"})

	host.submit("/replay", nil)
	waitIdle(t, host)

	if !strings.Contains(ui.transcript(), "nothing recorded to replay yet") {
		t.Errorf("replay of an unstarted run did not report empty:\n%s", ui.transcript())
	}
}
