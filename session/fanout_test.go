package session

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// TestProjectBuildsFanoutTree folds a fan-out run (the root spawns two children, each
// takes a turn, then folds back one clean and one failed) and checks the tree the live
// view reads: both children present in spawn order with their parent, objective, turn
// count, and terminal state.
func TestProjectBuildsFanoutTree(t *testing.T) {
	const root = "run-1"
	evs := []Event{
		{Kind: KindSessionStarted, Text: "split the work"},
		{Kind: KindTurnStarted, Goal: root, Turn: 1},
		{Kind: KindChildSpawned, Goal: root, Child: "child-a", Text: "do A", ToolUseID: "s1"},
		{Kind: KindChildSpawned, Goal: root, Child: "child-b", Text: "do B", ToolUseID: "s2"},
		// Each child runs a turn on its own goal id.
		{Kind: KindTurnCompleted, Goal: "child-a", Turn: 1, Usage: &Usage{OutputTokens: 5}},
		{Kind: KindTurnCompleted, Goal: "child-b", Turn: 1, Usage: &Usage{OutputTokens: 7}},
		// They fold back into the parent: A cleanly, B as a failure.
		{Kind: KindChildCompleted, Goal: root, Child: "child-a", Result: "A done", ToolUseID: "s1"},
		{Kind: KindChildCompleted, Goal: root, Child: "child-b", Result: "B broke", IsError: true, ToolUseID: "s2"},
		{Kind: KindTurnCompleted, Goal: root, Turn: 1, Usage: &Usage{OutputTokens: 3}},
		{Kind: KindConverged, Text: "folded both"},
	}
	p := Project(evs)

	if len(p.Fanout) != 2 {
		t.Fatalf("Fanout has %d children, want 2: %+v", len(p.Fanout), p.Fanout)
	}
	a, b := p.Fanout[0], p.Fanout[1]
	if a.ID != "child-a" || a.Parent != root || a.Objective != "do A" {
		t.Errorf("child A = %+v, want id child-a parent %s objective 'do A'", a, root)
	}
	if a.State != FanoutDone || a.Result != "A done" || a.Turns != 1 {
		t.Errorf("child A state/result/turns = %q/%q/%d, want done/'A done'/1", a.State, a.Result, a.Turns)
	}
	if b.State != FanoutFailed || b.Result != "B broke" || b.Turns != 1 {
		t.Errorf("child B state/result/turns = %q/%q/%d, want failed/'B broke'/1", b.State, b.Result, b.Turns)
	}
	// The three completed turns (two children plus the root) all count toward the run
	// total, while each child's own turn count stays 1.
	if p.Turns != 3 {
		t.Errorf("run Turns = %d, want 3", p.Turns)
	}
}

// TestFanoutRunningUntilFolded proves a spawned child reads as running until its
// completion folds back, so a live tree shows in-flight children.
func TestFanoutRunningUntilFolded(t *testing.T) {
	p := Project([]Event{
		{Kind: KindChildSpawned, Goal: "run-1", Child: "c1", Text: "work"},
		{Kind: KindTurnCompleted, Goal: "c1", Turn: 1},
	})
	if len(p.Fanout) != 1 || p.Fanout[0].State != FanoutRunning {
		t.Fatalf("child state = %+v, want a single running child", p.Fanout)
	}
	if p.Fanout[0].Turns != 1 {
		t.Errorf("running child Turns = %d, want 1", p.Fanout[0].Turns)
	}
}

// TestReduceDoesNotMutateFanout proves the reducer is pure over the tree: folding a
// completion into a projection does not mutate the slice a caller still holds from
// before the fold.
func TestReduceDoesNotMutateFanout(t *testing.T) {
	before := Project([]Event{{Kind: KindChildSpawned, Goal: "r", Child: "c1", Text: "w"}})
	_ = Reduce(before, Event{Kind: KindChildCompleted, Goal: "r", Child: "c1", Result: "done"})
	if before.Fanout[0].State != FanoutRunning || before.Fanout[0].Result != "" {
		t.Errorf("input tree mutated by a later fold: %+v", before.Fanout[0])
	}
}

// TestChildTurnsDedupedPerGoal proves the turn.started high-water mark is per goal: a
// child announcing its turn 1 is recorded even though the root already started turn 1
// on the same shared stream, so a fan-out's child turns are not swallowed as duplicates.
func TestChildTurnsDedupedPerGoal(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	s := New(log, bus.NewMemory(), WithID("run-1"))
	rep := s.Reporter()

	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Goal: "run-1", Turn: 1})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Goal: "child-a", Turn: 1})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Goal: "child-a", Turn: 1}) // retry, deduped
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Goal: "child-b", Turn: 1})

	events, err := History(ctx, log, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	byGoal := map[string]int{}
	for _, ev := range events {
		if ev.Kind == KindTurnStarted {
			byGoal[ev.Goal]++
		}
	}
	if byGoal["run-1"] != 1 || byGoal["child-a"] != 1 || byGoal["child-b"] != 1 {
		t.Fatalf("turn.started per goal = %v, want one each for run-1, child-a, child-b", byGoal)
	}
}

// TestChildSpawnEdgeRoundTrips proves the spawn edge and its goal attribution survive a
// write and read back through the stream, so the tree rebuilds from the durable record.
func TestChildSpawnEdgeRoundTrips(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	s := New(log, bus.NewMemory(), WithID("run-1"))
	rep := s.Reporter()

	rep.Report(ctx, mission.Event{Kind: mission.EventChildSpawned, Goal: "run-1", Child: "c1", Text: "sub-task", ToolUseID: "s1"})
	rep.Report(ctx, mission.Event{Kind: mission.EventChildCompleted, Goal: "run-1", Child: "c1", Result: "answer", ToolUseID: "s1"})

	events, err := History(ctx, log, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	p := Project(events)
	if len(p.Fanout) != 1 {
		t.Fatalf("Fanout after round trip = %+v, want one child", p.Fanout)
	}
	c := p.Fanout[0]
	if c.ID != "c1" || c.Parent != "run-1" || c.Objective != "sub-task" || c.State != FanoutDone || c.Result != "answer" {
		t.Errorf("round-tripped child = %+v", c)
	}
}
