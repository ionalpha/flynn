package chain

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// TestProvenanceOf reads a provenance declaration off a stream and reports its tiers.
func TestProvenanceOf(t *testing.T) {
	events := []spine.Event{
		{Type: "action.dispatched"},
		{Type: ProvenanceDeclared, Payload: map[string]any{
			ProvenanceHarnessKey:    "codex",
			ProvenanceEffectsKey:    TierEnforced,
			ProvenanceReasoningKey:  TierUnobserved,
			ProvenanceReplayableKey: false,
			ProvenanceAttestedKey:   7,
		}},
	}
	p, ok := ProvenanceOf(events)
	if !ok {
		t.Fatal("declaration present but not found")
	}
	if p.Harness != "codex" {
		t.Errorf("harness = %q, want codex", p.Harness)
	}
	if p.Effects != TierEnforced || p.Reasoning != TierUnobserved {
		t.Errorf("tiers = effects %q / reasoning %q, want enforced / unobserved", p.Effects, p.Reasoning)
	}
	if p.Replayable {
		t.Error("an external-harness run must be non-replayable")
	}
	if p.AttestedEvents != 7 {
		t.Errorf("attested events = %d, want 7", p.AttestedEvents)
	}
}

// TestProvenanceOfAttestedAfterJSONRoundTrip proves the attested tally survives the
// store: a payload that has been through JSON carries its numbers as float64, and a
// reader that only accepted int would silently report zero attested events on every
// sealed record, understating what the harness claimed.
func TestProvenanceOfAttestedAfterJSONRoundTrip(t *testing.T) {
	payload := map[string]any{
		ProvenanceHarnessKey:    "codex",
		ProvenanceEffectsKey:    TierEnforced,
		ProvenanceReasoningKey:  TierUnobserved,
		ProvenanceReplayableKey: false,
		ProvenanceAttestedKey:   7,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, isFloat := decoded[ProvenanceAttestedKey].(float64); !isFloat {
		t.Fatalf("precondition: want a float64 after the round trip, got %T", decoded[ProvenanceAttestedKey])
	}

	p, ok := ProvenanceOf([]spine.Event{{Type: ProvenanceDeclared, Payload: decoded}})
	if !ok {
		t.Fatal("declaration present but not found")
	}
	if p.AttestedEvents != 7 {
		t.Errorf("attested events = %d, want 7", p.AttestedEvents)
	}
	if p.Replayable {
		t.Error("an external-harness run must be non-replayable")
	}
}

// TestProvenanceOfAbsent proves a native run carries no declaration, so the reader
// reports absence rather than a zero-value provenance a caller might misread as real.
func TestProvenanceOfAbsent(t *testing.T) {
	if _, ok := ProvenanceOf([]spine.Event{{Type: "action.dispatched"}, {Type: OutcomeRecorded}}); ok {
		t.Fatal("a native stream must carry no provenance declaration")
	}
}
