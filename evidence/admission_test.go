package evidence

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/spine"
)

// TestAdmissionVerdictsAreRecordedAndTheGateIgnoresThem: each verdict lands on the run's
// stream as its own event type carrying the item, the refusal and the reason, and none of
// them reads back as a verification, so no admission can prove an item.
func TestAdmissionVerdictsAreRecordedAndTheGateIgnoresThem(t *testing.T) {
	log := spine.NewMemoryLog()
	ev := NewSpineEvidence(log)
	r := goalRes("run-admit")
	ctx := context.Background()

	refused := goal.AdmissionVerdict{Item: "aaaa", Refusal: goal.RefusedAlreadyPasses, Reason: "already passes", DryRun: true}
	admitted := goal.AdmissionVerdict{Item: "bbbb", Admitted: true, DryRun: true}
	for _, v := range []goal.AdmissionVerdict{refused, admitted} {
		if err := ev.RecordAdmission(ctx, r, v); err != nil {
			t.Fatalf("RecordAdmission: %v", err)
		}
	}

	events, err := log.Read(ctx, spine.Query{Stream: r.Name})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.Type != chain.ItemAdmitted || e.Actor != spine.ActorSystem {
			t.Fatalf("event = %s by %s, want %s by system", e.Type, e.Actor, chain.ItemAdmitted)
		}
	}
	p := events[0].Payload
	if p[chain.ItemKey] != "aaaa" || p[chain.ItemAdmittedKey] != false || p[chain.ItemRefusalKey] != "already_passes" || p[chain.ItemReasonKey] != "already passes" {
		t.Fatalf("refused payload = %v", p)
	}
	p = events[1].Payload
	if p[chain.ItemAdmittedKey] != true || p[chain.ItemDryRunKey] != true {
		t.Fatalf("admitted payload = %v", p)
	}
	if _, ok := p[chain.ItemRefusalKey]; ok {
		t.Fatalf("an admitted verdict carries a refusal: %v", p)
	}
	if vs := goal.VerificationsFrom(events); len(vs) != 0 {
		t.Fatalf("admission events read back as verifications: %+v", vs)
	}
}

// TestAnUnreachableRecordFailsAdmissionTransiently: a verdict that cannot be written is not
// silently dropped, since the plan would then run with nothing saying it was vetted.
func TestAnUnreachableRecordFailsAdmissionTransiently(t *testing.T) {
	err := NewSpineEvidence(brokenLog{}).RecordAdmission(context.Background(), goalRes("r"), goal.AdmissionVerdict{Item: "a"})
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("class = %v, want transient", fault.Classify(err))
	}
}
