package state_test

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// TestDecodeSkillReadsTheOldCounterAsOffers pins the one rule that makes the split
// survive a rebuild. Every skill event written before offers and reads were told
// apart carries a `Uses` key whose increments were impressions and a `Wins` key that
// counted those impressions on runs that converged. Replaying such an event has to
// land the first in Offers and drop the second: a database rebuilt from the stream
// then holds what the migration leaves behind, rather than resurrecting the counter
// this whole change exists to stop trusting.
func TestDecodeSkillReadsTheOldCounterAsOffers(t *testing.T) {
	old := json.RawMessage(`{"ID":"s1","Slug":"deploy","Uses":7,"Wins":5}`)
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"skill":`+string(old)+`}`), &raw); err != nil {
		t.Fatal(err)
	}
	sk, err := state.DecodeSkill(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sk.Offers != 7 {
		t.Errorf("Offers = %d, want 7: the old counter measured offers", sk.Offers)
	}
	if sk.Reads != 0 {
		t.Errorf("Reads = %d, want 0: no run before this split recorded whether it read a skill", sk.Reads)
	}
	if sk.Wins != 0 {
		t.Errorf("Wins = %d, want 0: offers on successful runs is not a fact about the skill", sk.Wins)
	}
}

// TestSkillEvidenceRoundTripsOnItsOwnKeys is the other half: what is written now
// comes back whole, and it does not write the key an older reader would take for
// outcome evidence. Reads is the count Confidence divides by, so a payload that put
// it back under `Uses` would hand a downgraded binary a win rate over impressions.
func TestSkillEvidenceRoundTripsOnItsOwnKeys(t *testing.T) {
	b, err := json.Marshal(state.Skill{Slug: "deploy", Offers: 9, Reads: 4, Wins: 3})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	if string(keys["Uses"]) != "9" {
		t.Errorf("the `Uses` key holds %s, want the offer count", keys["Uses"])
	}
	if string(keys["Reads"]) != "4" || string(keys["ReadWins"]) != "3" {
		t.Errorf("reads/wins written as %s/%s, want 4/3 on their own keys", keys["Reads"], keys["ReadWins"])
	}
	if _, present := keys["Wins"]; present {
		t.Error("the old `Wins` key was written again; it means something this record does not hold")
	}

	var back state.Skill
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Offers != 9 || back.Reads != 4 || back.Wins != 3 {
		t.Errorf("round trip = (%d,%d,%d), want (9,4,3)", back.Offers, back.Reads, back.Wins)
	}
}
