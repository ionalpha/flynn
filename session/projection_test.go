package session

import "testing"

func TestNewProjectionRecords(t *testing.T) {
	p := NewProjection()
	if p.Record != RecordRecording {
		t.Errorf("Record = %q, want %q", p.Record, RecordRecording)
	}
	if p.Terminal {
		t.Error("a fresh projection is not terminal")
	}
}

// TestProjectFoldsAStream folds a representative run (open, an admitted-then-completed
// action, a rejected action, two turns of usage, seal, and convergence) and checks the
// status the badge and panel read.
func TestProjectFoldsAStream(t *testing.T) {
	evs := []Event{
		{Kind: KindSessionStarted, Text: "ship the feature"},
		{Kind: KindActionAdmitted, Action: "bash", Call: 1, Trust: "agent"},
		{Kind: KindActionCompleted, Action: "bash", Call: 1},
		{Kind: KindActionRejected, Action: "write_file", Call: 2, Trust: "model", Fault: "capability_denied"},
		{Kind: KindTurnCompleted, Usage: &Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40}},
		{Kind: KindTurnCompleted, Usage: &Usage{InputTokens: 50, OutputTokens: 10}},
		{Kind: KindRecordSealed},
		{Kind: KindConverged, Text: "done"},
	}
	p := Project(evs)

	if p.Objective != "ship the feature" {
		t.Errorf("Objective = %q", p.Objective)
	}
	if p.Result != "done" || !p.Terminal {
		t.Errorf("Result = %q Terminal = %v, want done/true", p.Result, p.Terminal)
	}
	if p.Turns != 2 {
		t.Errorf("Turns = %d, want 2", p.Turns)
	}
	if p.Usage.InputTokens != 150 || p.Usage.OutputTokens != 30 || p.Usage.CacheReadTokens != 40 {
		t.Errorf("Usage = %+v, want in=150 out=30 cacheRead=40", p.Usage)
	}
	if p.Admitted != 1 || p.Completed != 1 || p.Rejected != 1 {
		t.Errorf("counts admitted=%d completed=%d rejected=%d, want 1/1/1", p.Admitted, p.Completed, p.Rejected)
	}
	// Containment posture is the most recent governed action's trust: the rejected
	// write, not the earlier admitted bash.
	if p.Containment != "model" {
		t.Errorf("Containment = %q, want model", p.Containment)
	}
	if p.Record != RecordSealed {
		t.Errorf("Record = %q, want sealed", p.Record)
	}
}

// TestProjectionRecordOrdering pins the record lifecycle ordering: verified outranks
// sealed, so a re-seal after verification never demotes the badge.
func TestProjectionRecordOrdering(t *testing.T) {
	sealedThenVerified := Project([]Event{{Kind: KindRecordSealed}, {Kind: KindRecordVerified}})
	if sealedThenVerified.Record != RecordVerified {
		t.Errorf("Record = %q, want verified", sealedThenVerified.Record)
	}
	verifiedThenSealed := Project([]Event{{Kind: KindRecordSealed}, {Kind: KindRecordVerified}, {Kind: KindRecordSealed}})
	if verifiedThenSealed.Record != RecordVerified {
		t.Errorf("re-seal demoted a verified record to %q", verifiedThenSealed.Record)
	}
}

func TestProjectionStall(t *testing.T) {
	p := Project([]Event{{Kind: KindSessionStarted, Text: "x"}, {Kind: KindStalled, Err: "out of budget"}})
	if !p.Terminal || p.Err != "out of budget" {
		t.Errorf("Terminal = %v Err = %q, want true/out of budget", p.Terminal, p.Err)
	}
	if p.Result != "" {
		t.Errorf("Result = %q, want empty on stall", p.Result)
	}
}

// TestReduceIsIncremental proves folding one event at a time (the live status line's
// path) matches Project over the whole slice (the replay path).
func TestReduceIsIncremental(t *testing.T) {
	evs := []Event{
		{Kind: KindActionAdmitted, Trust: "agent"},
		{Kind: KindTurnCompleted, Usage: &Usage{InputTokens: 10}},
		{Kind: KindRecordSealed},
	}
	incremental := NewProjection()
	for _, ev := range evs {
		incremental = Reduce(incremental, ev)
	}
	if incremental != Project(evs) {
		t.Errorf("incremental %+v != Project %+v", incremental, Project(evs))
	}
}
