package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
)

// TestClearResetsRun proves /clear forgets the current run and any carried summary, so
// the next turn opens a fresh conversation.
func TestClearResetsRun(t *testing.T) {
	s := &replSession{
		started: true, runID: "abc", lastSeq: 5,
		transcript:     []llm.Message{llm.Text(llm.RoleUser, "hi")},
		carriedContext: "old summary",
	}
	s.clear()
	if s.started || s.runID != "" || s.lastSeq != 0 || len(s.transcript) != 0 || s.carriedContext != "" {
		t.Fatalf("clear did not reset the run: %+v", s)
	}
}

// TestCompactSummarizesAndCarries proves /compact summarizes the transcript, resets the
// run, and carries the summary forward so the next run continues with less context.
func TestCompactSummarizesAndCarries(t *testing.T) {
	s := &replSession{
		started: true, model: constModel{text: "a tidy summary"},
		transcript: []llm.Message{llm.Text(llm.RoleUser, "do a thing"), llm.Text(llm.RoleAssistant, "did it")},
	}
	n, err := s.compact(context.Background())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 2 {
		t.Fatalf("compact reported %d messages, want 2", n)
	}
	if s.started || s.runID != "" {
		t.Fatal("compact did not reset the run")
	}
	if !strings.Contains(s.carriedContext, "a tidy summary") {
		t.Fatalf("carried context missing the summary: %q", s.carriedContext)
	}
}

// TestCompactNothingToDo proves compacting before any turn reports there is nothing to
// compact rather than making an empty model call.
func TestCompactNothingToDo(t *testing.T) {
	s := &replSession{model: constModel{text: "x"}}
	if _, err := s.compact(context.Background()); err == nil {
		t.Fatal("compact with no started run should error")
	}
}

// TestShellClearAndCompact drives both commands through the full-screen session: they run
// as commands (not model turns), reset the run, and report their outcome.
func TestShellClearAndCompact(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "the summary"})

	host.submit("do something", nil)
	waitIdle(t, host)
	if !host.s.started {
		t.Fatal("first turn did not start a run")
	}

	host.submit("/compact", nil)
	waitIdle(t, host)
	if host.s.started {
		t.Fatal("/compact did not reset the run")
	}
	if !strings.Contains(ui.transcript(), "compacted") {
		t.Fatalf("/compact was not reported:\n%s", ui.transcript())
	}
	if !strings.Contains(host.s.carriedContext, "the summary") {
		t.Fatalf("/compact did not carry the summary forward: %q", host.s.carriedContext)
	}

	host.submit("another", nil)
	waitIdle(t, host)
	host.submit("/clear", nil)
	waitIdle(t, host)
	if host.s.started || host.s.carriedContext != "" {
		t.Fatal("/clear did not reset the run and drop the summary")
	}
	if !strings.Contains(ui.transcript(), "context cleared") {
		t.Fatalf("/clear was not reported:\n%s", ui.transcript())
	}
}
