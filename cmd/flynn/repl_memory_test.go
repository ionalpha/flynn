package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/state"
)

// forkTheSubject writes two decisions whose subjects are spellings of each other, which
// is what the write policy reports: the second one is a fork of the first, caught at the
// moment it appears rather than after both chains exist.
func forkTheSubject(t *testing.T, s *replSession) {
	t.Helper()
	ctx := context.Background()
	for _, subject := range []string{"db-choice", "database-choice"} {
		if _, err := s.memory().store.Write(ctx, state.MemoryItem{
			Kind: "decision", Subject: subject, Content: "Postgres",
		}); err != nil {
			t.Fatalf("write %s: %v", subject, err)
		}
	}
}

// TestMemoryNoticesReachTheSessionsOutput proves a write the memory policy wants to say
// something about is surfaced to the operator rather than dropped. The session builds its
// memory once and hands it somewhere to report to; a stack built with nowhere to report
// silently loses the one thing this policy produces.
func TestMemoryNoticesReachTheSessionsOutput(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())

	forkTheSubject(t, s)

	if out := buf.String(); !strings.Contains(out, "memory:") || !strings.Contains(out, "database-choice") {
		t.Fatalf("the fork was not reported to the session:\n%s", out)
	}
}

// TestMemoryNoticesPreferTheSessionsNoticeLine proves the full-screen interface's notice
// line wins when there is one. Printing straight to the writer there would put the line
// in the middle of a rendered frame instead of where the session shows its notices.
func TestMemoryNoticesPreferTheSessionsNoticeLine(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	var noticed []string
	s.notice = func(text string) { noticed = append(noticed, text) }

	forkTheSubject(t, s)

	if len(noticed) == 0 {
		t.Fatal("the notice line was installed and never used")
	}
	if !strings.HasPrefix(noticed[0], "memory: ") {
		t.Fatalf("the notice does not say where it came from: %q", noticed[0])
	}
	if strings.Contains(buf.String(), "database-choice") {
		t.Fatalf("the notice was also printed into the transcript:\n%s", buf.String())
	}
}
