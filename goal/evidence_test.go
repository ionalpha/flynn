package goal

import (
	"errors"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// provenStatus is a status whose ledger carries items, all unproven, so a test can
// drive proofs through the gate against it.
func provenStatus(items ...LedgerItem) *Status {
	s := &Status{}
	s.SyncLedger(items)
	return s
}

// TestProveRefusesAClaimWithNoRecordedVerification is the default-FAIL heart of the
// gate: an item nothing was recorded for is refused, with a reason that names the
// missing evidence, and the ledger is left untouched so nothing is half-marked.
func TestProveRefusesAClaimWithNoRecordedVerification(t *testing.T) {
	g := newGate(t)
	a := item("ship the endpoint", "curl /health returns 200")
	s := provenStatus(a)

	err := s.Prove(g, a.ID, nil, testNow)
	if !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("a claim with no evidence: got %v, want ErrNoEvidence", err)
	}
	if s.LedgerSettled() || s.Ledger[0].Proven {
		t.Fatal("a refused claim still marked the item proven")
	}
}

// TestProveRefusesAFailingVerification: a verification that ran and failed is evidence
// the item is not done, not an absence of evidence to wave through. The gate refuses it.
func TestProveRefusesAFailingVerification(t *testing.T) {
	g := newGate(t)
	a := item("ship the endpoint", "curl /health returns 200")
	s := provenStatus(a)

	recorded := []Verification{{Ref: "7", Item: a.ID, Passed: false}}
	if err := s.Prove(g, a.ID, recorded, testNow); !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("a failing verification: got %v, want ErrNoEvidence", err)
	}
	if s.Ledger[0].Proven {
		t.Fatal("a failing verification proved the item")
	}
}

// TestProveAdmitsAndRecordsAPassingVerification: the one admitting path. A passing
// verification for the item flips it to proven and records the consumed ref as its
// evidence, which is what makes the consumption durable.
func TestProveAdmitsAndRecordsAPassingVerification(t *testing.T) {
	g := newGate(t)
	a := item("ship the endpoint", "curl /health returns 200")
	s := provenStatus(a)

	recorded := []Verification{{Ref: "42", Item: a.ID, Passed: true}}
	if err := s.Prove(g, a.ID, recorded, testNow); err != nil {
		t.Fatalf("a passing verification was refused: %v", err)
	}
	if !s.Ledger[0].Proven {
		t.Fatal("a passing verification did not prove the item")
	}
	if s.Ledger[0].Evidence != "42" {
		t.Fatalf("consumed ref not recorded as evidence: got %q, want %q", s.Ledger[0].Evidence, "42")
	}
	if !s.LedgerSettled() {
		t.Fatal("a one-item ledger with its only item proven is not settled")
	}
}

// TestEvidenceIsConsumedPerItem is the property the gate exists for beyond default-FAIL:
// one recorded verification cannot certify a second item. After it proves item a, the
// same ref is spent, so item b — which has only that same ref recorded against it — is
// refused as spent rather than riding a's proof.
func TestEvidenceIsConsumedPerItem(t *testing.T) {
	g := newGate(t)
	a := item("item a", "check a")
	b := item("item b", "check b")
	s := provenStatus(a, b)

	// One verification, ref "9". It legitimately proves a.
	provesA := []Verification{{Ref: "9", Item: a.ID, Passed: true}}
	if err := s.Prove(g, a.ID, provesA, testNow); err != nil {
		t.Fatalf("proving a: %v", err)
	}

	// Now b tries to lean on the very same ref (as if one screenshot certified two
	// features). The ref is spent, so b is refused.
	reusesRef := []Verification{{Ref: "9", Item: b.ID, Passed: true}}
	if err := s.Prove(g, b.ID, reusesRef, testNow); !errors.Is(err, ErrEvidenceSpent) {
		t.Fatalf("reusing a spent verification: got %v, want ErrEvidenceSpent", err)
	}
	if s.Ledger[1].Proven {
		t.Fatal("a spent verification proved a second item")
	}

	// b proven on its own fresh verification is fine: consumption bars reuse, not proof.
	provesB := []Verification{{Ref: "10", Item: b.ID, Passed: true}}
	if err := s.Prove(g, b.ID, provesB, testNow); err != nil {
		t.Fatalf("proving b on its own evidence: %v", err)
	}
	if !s.LedgerSettled() {
		t.Fatal("both items proven on distinct evidence but the ledger is not settled")
	}
}

// TestProveRefusesAVerificationForADifferentItem: evidence recorded against item a does
// not prove item b, because the gate matches on the item id — the content address of the
// item's text and its declared verify clause. A check for the wrong item is no evidence.
func TestProveRefusesAVerificationForADifferentItem(t *testing.T) {
	g := newGate(t)
	a := item("item a", "check a")
	b := item("item b", "check b")
	s := provenStatus(a, b)

	recorded := []Verification{{Ref: "3", Item: a.ID, Passed: true}}
	if err := s.Prove(g, b.ID, recorded, testNow); !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("evidence for a proving b: got %v, want ErrNoEvidence", err)
	}
}

// TestProveRefusesAnUnknownItem: a claim about work that was never planned is refused by
// the ledger beneath the gate, so the gate cannot be used to smuggle in a new item.
func TestProveRefusesAnUnknownItem(t *testing.T) {
	g := newGate(t)
	a := item("item a", "check a")
	s := provenStatus(a)

	// A passing verification, but for an id the ledger does not carry.
	recorded := []Verification{{Ref: "1", Item: "deadbeefdeadbeef", Passed: true}}
	if err := s.Prove(g, "deadbeefdeadbeef", recorded, testNow); !errors.Is(err, ErrLedgerUnknownItem) {
		t.Fatalf("proving an unplanned item: got %v, want ErrLedgerUnknownItem", err)
	}
}

// TestGateFailsLoudlyWhenItsOwnLogicBreaks is the guard the task calls for: the gate
// must fail loudly, not silently, if its own wiring breaks. selfTest is fed a decision
// that admits everything (the default-ALLOW a bad refactor would produce), and it must
// catch it and return ErrGateBroken rather than declaring the gate healthy.
func TestGateFailsLoudlyWhenItsOwnLogicBreaks(t *testing.T) {
	admitEverything := func(string, []Verification, map[string]bool) (string, error) {
		return "waved-through", nil
	}
	if err := selfTest(admitEverything, false); !errors.Is(err, ErrGateBroken) {
		t.Fatalf("selfTest passed a gate that admits everything: got %v, want ErrGateBroken", err)
	}

	// A gate that refuses everything (including fresh, valid evidence) is just as broken
	// and must also be caught, so the self-test cannot be satisfied by a gate that simply
	// never admits.
	refuseEverything := func(string, []Verification, map[string]bool) (string, error) {
		return "", ErrNoEvidence
	}
	if err := selfTest(refuseEverything, false); !errors.Is(err, ErrGateBroken) {
		t.Fatalf("selfTest passed a gate that refuses valid evidence: got %v, want ErrGateBroken", err)
	}
}

// TestNewEvidenceGatePassesItsOwnSelfTest: the real gate's decision satisfies the same
// self-test, so construction succeeds and the runtime gets a gate that has just proven it
// enforces the rule.
func TestNewEvidenceGatePassesItsOwnSelfTest(t *testing.T) {
	if _, err := NewEvidenceGate(); err != nil {
		t.Fatalf("the real gate failed its own self-test: %v", err)
	}
}

// TestVerificationsFromReadsItemChecksOffTheSpine proves the reader that turns recorded
// spine events into the gate's input: it picks up ItemVerified events with their verdict
// and identity, and skips a malformed one rather than trusting it — a verification the
// reader cannot make sense of must not become evidence.
func TestVerificationsFromReadsItemChecksOffTheSpine(t *testing.T) {
	events := []spine.Event{
		{Seq: 5, Type: chain.ItemVerified, Payload: map[string]any{chain.ItemKey: "aaaa", chain.ItemPassedKey: true}},
		{Seq: 6, Type: "some.other.event", Payload: map[string]any{chain.ItemKey: "bbbb"}},
		{Seq: 7, Type: chain.ItemVerified, Payload: map[string]any{chain.ItemKey: "cccc", chain.ItemPassedKey: false}},
		{Seq: 8, Type: chain.ItemVerified, Payload: map[string]any{chain.ItemPassedKey: true}}, // no item id: skipped
	}
	got := VerificationsFrom(events)
	// An event carrying no provenance is read at its weakest, as an assertion, rather
	// than as an unset kind the gate would have to interpret later.
	want := []Verification{
		{Ref: "5", Item: "aaaa", Passed: true, Provenance: ProvenanceAsserted},
		{Ref: "7", Item: "cccc", Passed: false, Provenance: ProvenanceAsserted},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d verifications, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("verification %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestProveConsumesEvidenceReadFromTheSpineEndToEnd wires the reader to the gate the way
// the runtime will: verifications recorded as spine events prove their items, and a
// second item cannot reuse the first's recorded check.
func TestProveConsumesEvidenceReadFromTheSpineEndToEnd(t *testing.T) {
	g := newGate(t)
	a := item("item a", "check a")
	b := item("item b", "check b")
	s := provenStatus(a, b)

	events := []spine.Event{
		{Seq: 11, Type: chain.ItemVerified, Payload: map[string]any{chain.ItemKey: a.ID, chain.ItemPassedKey: true}},
		{Seq: 12, Type: chain.ItemVerified, Payload: map[string]any{chain.ItemKey: b.ID, chain.ItemPassedKey: true}},
	}
	recorded := VerificationsFrom(events)

	if err := s.Prove(g, a.ID, recorded, testNow); err != nil {
		t.Fatalf("proving a from spine evidence: %v", err)
	}
	if err := s.Prove(g, b.ID, recorded, testNow); err != nil {
		t.Fatalf("proving b from spine evidence: %v", err)
	}
	if !s.LedgerSettled() {
		t.Fatal("both items have recorded passing checks but the ledger is not settled")
	}
	if s.Ledger[0].Evidence != "11" || s.Ledger[1].Evidence != "12" {
		t.Fatalf("wrong evidence consumed: a=%q b=%q", s.Ledger[0].Evidence, s.Ledger[1].Evidence)
	}
}
