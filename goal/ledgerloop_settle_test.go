package goal

// Settling: when an item flips to proven, and what the run says about the ones that did
// not. An item settles only from a record the gate admitted, and it spends its budget
// once. The refusals are reported apart rather than collapsed into "not done", because
// an unrunnable check and an unattempted one need opposite responses from a caller.

import (
	"strings"
	"testing"
)

// TestProveRecordedSettlesFromTheRecordAndSpendsOnce: an item flips to proven only from a
// verification on the record, and that verification is spent, so one recorded check cannot
// certify two items.
func TestProveRecordedSettlesFromTheRecordAndSpendsOnce(t *testing.T) {
	ledger := twoItemLedger(t)
	var st Status
	st.SyncLedger(ledger)
	gate := newGate(t, RequireExecuted())
	now := testNow

	if n := st.ProveRecorded(gate, nil, now); n != 0 {
		t.Fatalf("proved %d items from an empty record, want 0", n)
	}

	recorded := []Verification{{Ref: "9", Item: ledger[0].ID, Passed: true, Provenance: ProvenanceExecuted}}
	if n := st.ProveRecorded(gate, recorded, now); n != 1 {
		t.Fatalf("proved %d items, want 1", n)
	}
	if st.Ledger[0].Evidence != "9" {
		t.Fatalf("item 1 evidence = %q, want the verification ref", st.Ledger[0].Evidence)
	}
	if st.LedgerSettled() {
		t.Fatal("a ledger with one item still unproven reported settled")
	}

	// A second pass over the same record must not re-spend the consumed verification on
	// the remaining item, whose id it does not name in any case.
	if n := st.ProveRecorded(gate, recorded, now); n != 0 {
		t.Fatalf("a repeat settling pass proved %d more items, want 0", n)
	}

	recorded = append(recorded, Verification{Ref: "10", Item: ledger[1].ID, Passed: true, Provenance: ProvenanceExecuted})
	if n := st.ProveRecorded(gate, recorded, now); n != 1 {
		t.Fatalf("proved %d items on the second check, want 1", n)
	}
	if !st.LedgerSettled() {
		t.Fatal("a ledger with every item proven did not report settled")
	}
}

// TestUnprovenReasonsDistinguishTheThreeRefusals: a run record that flattened these into
// "not done" would lose the difference between an item nobody checked, one whose check was
// spent elsewhere, and one whose check could not be run: three problems with three fixes.
func TestUnprovenReasonsDistinguishTheThreeRefusals(t *testing.T) {
	ledger, err := AppendItems(nil,
		LedgerItem{Item: "nothing checked this", Verify: "true"},
		LedgerItem{Item: "its check was spent", Verify: "false"},
		LedgerItem{Item: "its check was only claimed", Verify: "echo hi"},
		LedgerItem{Item: "already done", Verify: "test -f go.mod"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	st.SyncLedger(ledger)
	// The settled item consumed ref "5", which is also the only passing verification the
	// second item has: that is what a spent refusal looks like from the record's side.
	if err := st.MarkProven(ledger[3].ID, "5", testNow); err != nil {
		t.Fatal(err)
	}
	recorded := []Verification{
		{Ref: "5", Item: ledger[1].ID, Passed: true, Provenance: ProvenanceExecuted},
		{Ref: "6", Item: ledger[2].ID, Passed: true, Provenance: ProvenanceAsserted},
	}

	reasons := st.UnprovenReasons(newGate(t, RequireExecuted()), recorded)
	if len(reasons) != 3 {
		t.Fatalf("got %d reasons, want one per unproven item: %v", len(reasons), reasons)
	}
	want := []string{
		"no recorded passing verification",
		"already consumed by another item",
		"asserted, not executed",
	}
	for i, w := range want {
		if !strings.Contains(reasons[i], w) {
			t.Fatalf("reason %d = %q, want it to mention %q", i, reasons[i], w)
		}
		if !strings.HasPrefix(reasons[i], ledger[i].ID) {
			t.Fatalf("reason %d = %q, want it to name item %s", i, reasons[i], ledger[i].ID)
		}
	}
	if (Status{}).UnprovenReasons(newGate(t), nil) != nil {
		t.Fatal("a goal with no ledger produced unproven reasons")
	}
}

// TestUnprovenReasonReportsAnAdmissibleItemAsTheAnomalyItIs: reaching the refusal with an
// item the gate would have admitted means the settling pass did not settle something it
// could have, which is a bug in the loop rather than an item nobody did the work for. It is
// reported as such instead of being papered over as "not done".
func TestUnprovenReasonReportsAnAdmissibleItemAsTheAnomalyItIs(t *testing.T) {
	if got := unprovenReason(nil, "x", nil); got != "admissible but not settled" {
		t.Fatalf("reason for a nil refusal = %q", got)
	}
}

// TestAnUnrunnableCheckIsNotReportedAsUnchecked: both reach the gate as ErrNoEvidence,
// because an unrunnable check records a verdict that did not pass exactly as a failing one
// does. They need opposite responses, though: one is work still to do, the other is a check
// the host or the clause cannot execute, and no amount of further building fixes it.
func TestAnUnrunnableCheckIsNotReportedAsUnchecked(t *testing.T) {
	ledger, err := AppendItems(nil,
		LedgerItem{Item: "nobody checked this", Verify: "true"},
		LedgerItem{Item: "its check ran and failed", Verify: "false"},
		LedgerItem{Item: "its check could not run here", Verify: "a command this host cannot run"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	st.SyncLedger(ledger)
	recorded := []Verification{
		{Ref: "1", Item: ledger[1].ID, Passed: false, Provenance: ProvenanceExecuted},
		{Ref: "2", Item: ledger[2].ID, Passed: false, Provenance: ProvenanceAsserted},
	}

	reasons := st.UnprovenReasons(newGate(t, RequireExecuted()), recorded)
	want := []string{
		"no recorded passing verification",
		"its check ran and did not pass",
		"its check could not be run",
	}
	for i, w := range want {
		if !strings.Contains(reasons[i], w) {
			t.Fatalf("reason %d = %q, want it to say %q", i, reasons[i], w)
		}
	}
}

// TestAnUnrunnableCheckSaysWhyItCouldNotRun: "could not be run" names no cause, so a clause
// no host could ever execute and a sandbox that failed to start read identically to whoever
// is handed the stopped goal, and only one of those is worth trying to fix. The verifier
// knows which it was; this is the path that carries it to a reader.
func TestAnUnrunnableCheckSaysWhyItCouldNotRun(t *testing.T) {
	ledger, err := AppendItems(nil, LedgerItem{Item: "its check could not run here", Verify: "make test"})
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	st.SyncLedger(ledger)
	const why = "the check could not run: the sandbox refused to start"
	recorded := []Verification{
		{Ref: "1", Item: ledger[0].ID, Passed: false, Provenance: ProvenanceAsserted, Reason: why},
	}

	reasons := st.UnprovenReasons(newGate(t, RequireExecuted()), recorded)
	if len(reasons) != 1 || !strings.Contains(reasons[0], why) {
		t.Fatalf("reasons = %v, want the one item's refusal to quote %q", reasons, why)
	}
}
