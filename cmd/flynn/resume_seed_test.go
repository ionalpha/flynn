package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/session"
)

// TestShellResumeSeedsLikeLive proves a resumed run is seeded through the live
// transcript renderer and folded into the badge projection: the conversation and its
// answer appear, the verbose goal-style "turn N"/"goal:" lines do not, and the run's
// turn count reaches the projection so the badge shows it rather than starting at zero.
func TestShellResumeSeedsLikeLive(t *testing.T) {
	host1, _ := newHostForTest(t, constModel{text: "the answer is 42"})
	host1.submit("what is the meaning", nil)
	waitIdle(t, host1)
	runID := host1.s.runID
	if runID == "" {
		t.Fatal("first turn did not start a run")
	}

	// A second host over the same store resumes that run.
	s2, _ := newREPL(t, t.TempDir(), host1.s.store, constModel{text: "unused"})
	s2.started = true
	s2.runID = runID
	ui := &fakeUI{}
	th := theme.Default()
	host2 := &sessionHost{
		ctx: host1.ctx, s: s2, ui: ui, th: th,
		tv: newTranscriptView(th), live: &activity{th: th},
		panel: &govPanel{th: th}, approval: &approvalPrompt{th: th},
		proj: session.NewProjection(),
	}

	host2.greet()

	got := ui.transcript()
	if !strings.Contains(got, "what is the meaning") || !strings.Contains(got, "the answer is 42") {
		t.Fatalf("resume did not seed the conversation and its answer:\n%s", got)
	}
	if strings.Contains(got, "turn 1") || strings.Contains(got, "goal:") {
		t.Fatalf("resume showed verbose goal-style lines instead of the live transcript:\n%s", got)
	}
	if host2.proj.Turns == 0 {
		t.Fatal("resume did not fold history into the projection; the badge would show 0 turns")
	}
}
