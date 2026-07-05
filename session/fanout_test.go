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

// TestFanoutPerChildGovernance proves a governed action folds into the child that ran
// it: a capability denial on one child raises that child's blocked count and sets its
// trust, while a sibling that ran a clean action shows its trust with no block. A root
// action attributes to no child and moves only the run-level containment.
func TestFanoutPerChildGovernance(t *testing.T) {
	const root = "run-1"
	p := Project([]Event{
		{Kind: KindSessionStarted, Text: "split the work"},
		{Kind: KindChildSpawned, Goal: root, Child: "child-a", Text: "do A"},
		{Kind: KindChildSpawned, Goal: root, Child: "child-b", Text: "do B"},
		// child-a is refused a write under model trust; child-b runs a read cleanly.
		{Kind: KindActionAdmitted, Goal: "child-a", Call: 1, Action: "write_file", Trust: "model"},
		{Kind: KindActionRejected, Goal: "child-a", Call: 1, Action: "write_file", Trust: "model", Fault: "capability_denied"},
		{Kind: KindActionAdmitted, Goal: "child-b", Call: 2, Action: "read_file", Trust: "agent"},
		{Kind: KindActionCompleted, Goal: "child-b", Call: 2, Action: "read_file", Trust: "agent"},
		// A root action attributes to no child.
		{Kind: KindActionAdmitted, Goal: root, Call: 3, Action: "model.generate", Trust: "agent"},
	})

	if len(p.Fanout) != 2 {
		t.Fatalf("Fanout has %d children, want 2: %+v", len(p.Fanout), p.Fanout)
	}
	a, b := p.Fanout[0], p.Fanout[1]
	if a.Blocked != 1 || a.Trust != "model" {
		t.Errorf("child A governance = %d blocked / trust %q, want 1 / model", a.Blocked, a.Trust)
	}
	if b.Blocked != 0 || b.Trust != "agent" {
		t.Errorf("child B governance = %d blocked / trust %q, want 0 / agent", b.Blocked, b.Trust)
	}
	// The run-level tallies still count every action; the root action set containment.
	if p.Rejected != 1 || p.Admitted != 3 {
		t.Errorf("run tallies admitted/rejected = %d/%d, want 3/1", p.Admitted, p.Rejected)
	}
	if p.Containment != "agent" {
		t.Errorf("run Containment = %q, want agent (the last admitted action's trust)", p.Containment)
	}
}

// TestReduceDoesNotMutateChildGovernance proves folding a governed action into a child is
// pure: it does not mutate the tree slice a caller still holds from before the fold.
func TestReduceDoesNotMutateChildGovernance(t *testing.T) {
	before := Project([]Event{{Kind: KindChildSpawned, Goal: "r", Child: "c1", Text: "w"}})
	_ = Reduce(before, Event{Kind: KindActionRejected, Goal: "c1", Call: 1, Trust: "model", Fault: "capability_denied"})
	if before.Fanout[0].Blocked != 0 || before.Fanout[0].Trust != "" {
		t.Errorf("input tree mutated by a later governance fold: %+v", before.Fanout[0])
	}
}

// TestFanoutPerChildSealState proves each folded child's seal state tracks the run's
// record: recording while the run records, sealed once the run's stream is signed (the
// child's events are then under the signed root), verified once the run is verified.
func TestFanoutPerChildSealState(t *testing.T) {
	const root = "run-1"
	base := []Event{
		{Kind: KindSessionStarted, Text: "split the work"},
		{Kind: KindChildSpawned, Goal: root, Child: "a", Text: "do A"},
		{Kind: KindChildSpawned, Goal: root, Child: "b", Text: "do B"},
		{Kind: KindChildCompleted, Goal: root, Child: "a", Result: "A done"},
		{Kind: KindChildCompleted, Goal: root, Child: "b", Result: "B broke", IsError: true},
	}
	// While the run records, both folded children read as recording.
	p := Project(base)
	if p.Fanout[0].Seal != RecordRecording || p.Fanout[1].Seal != RecordRecording {
		t.Fatalf("child seal before run seal = %q/%q, want recording", p.Fanout[0].Seal, p.Fanout[1].Seal)
	}
	// Sealing the run seals both folded children (a clean fold and a failed one alike).
	p = Reduce(p, Event{Kind: KindRecordSealed})
	if p.Fanout[0].Seal != RecordSealed || p.Fanout[1].Seal != RecordSealed {
		t.Fatalf("child seal after run seal = %q/%q, want sealed", p.Fanout[0].Seal, p.Fanout[1].Seal)
	}
	// Verifying the run verifies them.
	p = Reduce(p, Event{Kind: KindRecordVerified})
	if p.Fanout[0].Seal != RecordVerified || p.Fanout[1].Seal != RecordVerified {
		t.Fatalf("child seal after run verify = %q/%q, want verified", p.Fanout[0].Seal, p.Fanout[1].Seal)
	}
}

// TestFanoutRunningChildNotSealed proves a child still running when a seal lands stays
// recording, since its events are not yet final, while a sibling that folded before the
// seal is sealed. This is the genuine per-child seal variation.
func TestFanoutRunningChildNotSealed(t *testing.T) {
	p := Project([]Event{
		{Kind: KindChildSpawned, Goal: "run-1", Child: "a", Text: "do A"},
		{Kind: KindChildCompleted, Goal: "run-1", Child: "a", Result: "A done"},
		{Kind: KindChildSpawned, Goal: "run-1", Child: "b", Text: "do B"}, // still running at seal
		{Kind: KindRecordSealed},
	})
	if p.Fanout[0].Seal != RecordSealed {
		t.Errorf("folded child seal = %q, want sealed", p.Fanout[0].Seal)
	}
	if p.Fanout[1].Seal != RecordRecording {
		t.Errorf("running child seal = %q, want recording (its events are not final)", p.Fanout[1].Seal)
	}
}

// TestReduceDoesNotMutateChildSeal proves advancing the run seal is pure over the tree:
// it does not mutate the slice a caller still holds from before the seal.
func TestReduceDoesNotMutateChildSeal(t *testing.T) {
	before := Project([]Event{
		{Kind: KindChildSpawned, Goal: "r", Child: "c1", Text: "w"},
		{Kind: KindChildCompleted, Goal: "r", Child: "c1", Result: "done"},
	})
	_ = Reduce(before, Event{Kind: KindRecordSealed})
	if before.Fanout[0].Seal != RecordRecording {
		t.Errorf("input tree mutated by a later seal: %+v", before.Fanout[0])
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
