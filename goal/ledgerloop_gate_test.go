package goal

// What the gate admits. Two axes, both default-FAIL: a verification must exist and pass,
// and its provenance must be good enough for the policy in force, where an unknown value
// reads as merely asserted. The gate is self-tested as well as tested, because a gate
// that refuses the evidence the producer actually ran would stall every goal silently.

import (
	"errors"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// TestCurrentItemIsFirstUnproven: the run's current item is derived from the ledger and
// the per-item state, not stored, so there is no second representation of where the run
// is that could drift from the record.
func TestCurrentItemIsFirstUnproven(t *testing.T) {
	ledger := twoItemLedger(t)
	var st Status
	st.SyncLedger(ledger)

	item, ok := st.CurrentItem(ledger)
	if !ok || item.ID != ledger[0].ID {
		t.Fatalf("current item = %+v (ok=%v), want the first item", item, ok)
	}

	if err := st.MarkProven(ledger[0].ID, "1", testNow); err != nil {
		t.Fatal(err)
	}
	item, ok = st.CurrentItem(ledger)
	if !ok || item.ID != ledger[1].ID {
		t.Fatalf("after proving item 1, current = %+v (ok=%v), want item 2", item, ok)
	}

	if err := st.MarkProven(ledger[1].ID, "2", testNow); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.CurrentItem(ledger); ok {
		t.Fatal("a fully proven ledger still reported a current item")
	}
	if _, ok := (Status{}).CurrentItem(nil); ok {
		t.Fatal("an empty ledger reported a current item")
	}
}

// TestProvenanceReadsUnknownValuesAsAsserted: default-FAIL applied to the second axis. An
// event with a verdict but no readable provenance carries everything the original contract
// had, so it is not skipped, but it is taken at its weakest reading, because the only
// thing that may buy an event the executed kind is that exact value, present and readable.
func TestProvenanceReadsUnknownValuesAsAsserted(t *testing.T) {
	base := map[string]any{chain.ItemKey: "abc", chain.ItemPassedKey: true}
	cases := []struct {
		name string
		prov any
		want Provenance
	}{
		{"absent", nil, ProvenanceAsserted},
		{"asserted", chain.ProvenanceAsserted, ProvenanceAsserted},
		{"executed", chain.ProvenanceExecuted, ProvenanceExecuted},
		{"not a string", 7, ProvenanceAsserted},
		{"a value this build does not know", "attested-by-a-future-scheme", ProvenanceAsserted},
		{"a near miss", "Executed", ProvenanceAsserted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			for k, v := range base {
				payload[k] = v
			}
			if tc.prov != nil {
				payload[chain.ItemProvenanceKey] = tc.prov
			}
			got := VerificationsFrom([]spine.Event{{Seq: 3, Type: chain.ItemVerified, Payload: payload}})
			if len(got) != 1 {
				t.Fatalf("read %d verifications, want 1", len(got))
			}
			if got[0].Provenance != tc.want {
				t.Fatalf("provenance = %q, want %q", got[0].Provenance, tc.want)
			}
			if got[0].Ref != "3" {
				t.Fatalf("ref = %q, want the event sequence", got[0].Ref)
			}
		})
	}
}

// TestGateRequiringExecutionRefusesAnAssertion: the invariant that makes the provenance
// axis decide something rather than label it. There is no promotion and no override, so an
// unrunnable check fails its item instead of quietly passing it.
func TestGateRequiringExecutionRefusesAnAssertion(t *testing.T) {
	const item = "item0000000000aa"
	asserted := []Verification{{Ref: "1", Item: item, Passed: true, Provenance: ProvenanceAsserted}}
	executed := []Verification{{Ref: "2", Item: item, Passed: true, Provenance: ProvenanceExecuted}}

	strict := newGate(t, RequireExecuted())
	if _, err := strict.admit(item, asserted, nil); !errors.Is(err, ErrEvidenceAsserted) {
		t.Fatalf("an assertion satisfied a gate requiring execution: %v", err)
	}
	if _, err := strict.admit(item, executed, nil); err != nil {
		t.Fatalf("an executed check was refused by a strict gate: %v", err)
	}

	// A gate that was not configured for it must still admit an assertion, so turning
	// the policy on stays a decision rather than something the gate does on its own.
	lax := newGate(t)
	if _, err := lax.admit(item, asserted, nil); err != nil {
		t.Fatalf("a permissive gate refused an assertion: %v", err)
	}
}

// TestGateSelfTestCatchesADeletedProvenanceRefusal: the self-test is what makes the gate
// unable to ship broken, so it has to catch the provenance branch going missing too. A
// gate whose execution requirement had been refactored away would otherwise construct
// cleanly and wave assertions through at runtime.
func TestGateSelfTestCatchesADeletedProvenanceRefusal(t *testing.T) {
	// A decision that enforces every original rule but ignores provenance entirely.
	ignoresProvenance := func(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
		g := &EvidenceGate{} // requireExecuted deliberately off
		return g.admit(itemID, recorded, consumed)
	}
	if err := selfTest(ignoresProvenance, true); !errors.Is(err, ErrGateBroken) {
		t.Fatalf("selfTest passed a gate that ignores its execution requirement: %v", err)
	}
	if err := selfTest(ignoresProvenance, false); err != nil {
		t.Fatalf("selfTest failed a correct permissive gate: %v", err)
	}
}

// TestGateSelfTestCatchesARefusalOfExecutedEvidence: the provenance axis must never become a
// way to refuse the evidence the producer actually ran, so a gate that rejects an executed
// check is caught under either policy.
func TestGateSelfTestCatchesARefusalOfExecutedEvidence(t *testing.T) {
	refusesExecuted := func(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
		kept := make([]Verification, 0, len(recorded))
		for _, v := range recorded {
			if v.Provenance != ProvenanceExecuted {
				kept = append(kept, v)
			}
		}
		g := &EvidenceGate{}
		return g.admit(itemID, kept, consumed)
	}
	for _, requireExecuted := range []bool{false, true} {
		if err := selfTest(refusesExecuted, requireExecuted); !errors.Is(err, ErrGateBroken) {
			t.Fatalf("selfTest passed a gate that refuses executed evidence (requireExecuted=%v): %v", requireExecuted, err)
		}
	}
}
