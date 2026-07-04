package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/session"
)

// TestShellForkBranchesRun drives a turn, forks, then runs another turn on the branch. It
// proves the fork is a new run seeded with a verbatim copy of the conversation, records
// its parent's id, continues on its own stream, and never disturbs the original run.
func TestShellForkBranchesRun(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "first answer"})

	host.submit("first prompt", nil)
	waitIdle(t, host)
	parentID := host.s.runID

	host.submit("/fork", nil)
	waitIdle(t, host)
	forkID := host.s.runID

	if forkID == "" || forkID == parentID {
		t.Fatalf("fork did not switch to a new run: parent=%q fork=%q", parentID, forkID)
	}
	if !strings.Contains(ui.transcript(), "forked to run "+forkID) {
		t.Errorf("transcript missing fork confirmation:\n%s", ui.transcript())
	}

	ctx := context.Background()
	rs := host.s.store.Resources(host.s.reg)
	parent, err := rs.Get(ctx, goal.Kind, resource.Scope{}, parentID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	fork, err := rs.Get(ctx, goal.Kind, resource.Scope{}, forkID)
	if err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if fork.ID == "" || fork.ID == parent.ID {
		t.Errorf("fork identity not freshly stamped: parent.ID=%q fork.ID=%q", parent.ID, fork.ID)
	}
	if got := fork.Annotations[forkParentAnnotation]; got != parentID {
		t.Errorf("fork parent annotation = %q, want %q", got, parentID)
	}
	if string(fork.Status) != string(parent.Status) {
		t.Errorf("fork checkpoint is not a verbatim copy of the parent\nparent: %s\nfork:   %s", parent.Status, fork.Status)
	}

	// The original run's recorded history is frozen at the turn it was forked from.
	parentHist, err := session.History(ctx, host.s.store.Log(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	parentLen := len(parentHist)

	host.submit("second prompt", nil)
	waitIdle(t, host)

	if host.s.runID != forkID {
		t.Errorf("second turn left the fork: runID=%q want %q", host.s.runID, forkID)
	}
	forkHist, err := session.History(ctx, host.s.store.Log(), forkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(forkHist) == 0 {
		t.Error("fork recorded no events after its turn")
	}
	afterParent, err := session.History(ctx, host.s.store.Log(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterParent) != parentLen {
		t.Errorf("original run history changed after forking: was %d, now %d", parentLen, len(afterParent))
	}
}

// TestForkBeforeTurnReports proves forking before any turn has run reports there is
// nothing to branch from rather than creating an empty run.
func TestForkBeforeTurnReports(t *testing.T) {
	s, _ := newREPL(t, t.TempDir(), memStore(t), constModel{text: "unused"})
	if _, err := s.fork(context.Background()); err == nil {
		t.Fatal("fork before a turn should error")
	}
}
