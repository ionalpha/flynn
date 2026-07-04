package session

import (
	"reflect"
	"testing"
)

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

// TestLedgerUpsertsByCall proves an admission and its outcome share one ledger row: a
// completion updates the entry the admission created (matched by Call) rather than
// adding a second row, and keeps the name the admission recorded when the completion
// carries none.
func TestLedgerUpsertsByCall(t *testing.T) {
	p := Project([]Event{
		{Kind: KindActionAdmitted, Action: "bash", Call: 7, Trust: "agent"},
		{Kind: KindActionCompleted, Call: 7},
	})
	if len(p.Actions) != 1 {
		t.Fatalf("Actions = %d rows, want 1", len(p.Actions))
	}
	got := p.Actions[0]
	if got.State != ActionDone || got.Action != "bash" || got.Trust != "agent" {
		t.Errorf("entry = %+v, want done/bash/agent", got)
	}
}

// TestLedgerBlockedCarriesFault proves a refused action lands in the ledger as blocked
// with its fault class, the boundary the panel highlights.
func TestLedgerBlockedCarriesFault(t *testing.T) {
	p := Project([]Event{{Kind: KindActionRejected, Action: "write_file", Call: 3, Trust: "model", Fault: "capability_denied"}})
	if len(p.Actions) != 1 || p.Actions[0].State != ActionBlocked || p.Actions[0].Fault != "capability_denied" {
		t.Errorf("Actions = %+v, want one blocked capability_denied", p.Actions)
	}
}

// TestLedgerZeroCallAppends proves an action a waist records with no correlation id
// (Call zero) always appends rather than colliding with an earlier zero-Call entry, so
// two unkeyed actions stay two rows.
func TestLedgerZeroCallAppends(t *testing.T) {
	p := Project([]Event{
		{Kind: KindActionAdmitted, Action: "a"},
		{Kind: KindActionAdmitted, Action: "b"},
	})
	if len(p.Actions) != 2 {
		t.Errorf("Actions = %d rows, want 2 (zero-Call entries never merge)", len(p.Actions))
	}
}

// TestLedgerIsBounded proves the ledger keeps only its most recent maxLedger entries, so
// a long run's projection stays a fixed size, and the newest action survives.
func TestLedgerIsBounded(t *testing.T) {
	p := NewProjection()
	for i := range maxLedger + 20 {
		p = Reduce(p, Event{Kind: KindActionAdmitted, Action: "x", Call: int64(i + 1)})
	}
	if len(p.Actions) != maxLedger {
		t.Fatalf("Actions = %d rows, want capped at %d", len(p.Actions), maxLedger)
	}
	if last := p.Actions[len(p.Actions)-1]; last.Call != int64(maxLedger+20) {
		t.Errorf("newest entry Call = %d, want %d", last.Call, maxLedger+20)
	}
}

// TestReduceDoesNotMutateInputLedger proves the reducer is pure with respect to the
// ledger: folding an event into a projection leaves an earlier copy's slice unchanged,
// so a replay comparing successive states is not corrupted by aliasing.
func TestReduceDoesNotMutateInputLedger(t *testing.T) {
	before := Reduce(NewProjection(), Event{Kind: KindActionAdmitted, Action: "a", Call: 1})
	_ = Reduce(before, Event{Kind: KindActionCompleted, Call: 1})
	if before.Actions[0].State != ActionRunning {
		t.Errorf("folding mutated an earlier projection's ledger: state = %q, want running", before.Actions[0].State)
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
	if !reflect.DeepEqual(incremental, Project(evs)) {
		t.Errorf("incremental %+v != Project %+v", incremental, Project(evs))
	}
}
