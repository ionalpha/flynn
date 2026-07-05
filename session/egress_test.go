package session

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// egressPayload builds the egress sink's payload shape, the wire contract this package
// projects into KindEgressDecision.
func egressPayload(host, verdict, reason string) map[string]any {
	return map[string]any{"host": host, "verdict": verdict, "reason": reason}
}

// TestDecodeEgressEvent checks a net.egress spine event projects into the session
// vocabulary with its host, verdict, and reason, and that the verdict string maps to
// the Allowed bool.
func TestDecodeEgressEvent(t *testing.T) {
	allowed := fromSpine(spine.Event{Type: typeEgress, Payload: egressPayload("8.8.8.8", "allowed", "public")})
	if allowed.Kind != KindEgressDecision || !allowed.Allowed || allowed.Host != "8.8.8.8" || allowed.Reason != "public" {
		t.Fatalf("allowed egress = %+v", allowed)
	}
	blocked := fromSpine(spine.Event{Type: typeEgress, Payload: egressPayload("10.0.0.1", "blocked", "private or reserved address")})
	if blocked.Kind != KindEgressDecision || blocked.Allowed || blocked.Host != "10.0.0.1" {
		t.Fatalf("blocked egress = %+v", blocked)
	}
}

// TestEgressReducerFoldsDecisions folds an allow and a block into the projection and
// checks the counts and ledger both reflect the two decisions.
func TestEgressReducerFoldsDecisions(t *testing.T) {
	p := NewProjection()
	p = Reduce(p, Event{Kind: KindEgressDecision, Host: "8.8.8.8", Allowed: true, Reason: "public"})
	p = Reduce(p, Event{Kind: KindEgressDecision, Host: "10.0.0.1", Allowed: false, Reason: "private or reserved address"})

	if p.EgressAllowed != 1 || p.EgressBlocked != 1 {
		t.Fatalf("egress counts = %d allowed, %d blocked, want 1/1", p.EgressAllowed, p.EgressBlocked)
	}
	if len(p.Egress) != 2 {
		t.Fatalf("egress ledger = %d entries, want 2", len(p.Egress))
	}
	if p.Egress[1].Host != "10.0.0.1" || p.Egress[1].Allowed || p.Egress[1].Reason == "" {
		t.Fatalf("second egress entry = %+v, want the block with a reason", p.Egress[1])
	}
}

// TestEgressLedgerIsBounded checks the egress ledger keeps only its most recent
// maxEgress entries while the counts keep tallying every decision.
func TestEgressLedgerIsBounded(t *testing.T) {
	p := NewProjection()
	const n = maxEgress + 20
	for range n {
		p = Reduce(p, Event{Kind: KindEgressDecision, Host: "8.8.8.8", Allowed: true, Reason: "public"})
	}
	if len(p.Egress) != maxEgress {
		t.Fatalf("egress ledger = %d, want capped at %d", len(p.Egress), maxEgress)
	}
	if p.EgressAllowed != n {
		t.Fatalf("egress allowed count = %d, want %d (counts are not capped)", p.EgressAllowed, n)
	}
}

// TestReduceDoesNotMutateInputEgress checks folding an egress event leaves the caller's
// prior projection slice untouched, so the reducer stays pure.
func TestReduceDoesNotMutateInputEgress(t *testing.T) {
	before := Reduce(NewProjection(), Event{Kind: KindEgressDecision, Host: "8.8.8.8", Allowed: true})
	snapshot := append([]EgressEntry(nil), before.Egress...)
	_ = Reduce(before, Event{Kind: KindEgressDecision, Host: "10.0.0.1", Allowed: false})
	if len(before.Egress) != len(snapshot) {
		t.Fatalf("input ledger grew to %d, want %d", len(before.Egress), len(snapshot))
	}
}

// TestHistoryProjectsEgressEvents drives an egress event through the durable log and
// History, so the whole path (append, read, decode, project) folds it end to end.
func TestHistoryProjectsEgressEvents(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	const stream = "run-egress"
	appends := []spine.AppendInput{
		{Stream: stream, Type: string(KindSessionStarted), Payload: map[string]any{payloadKey: `{"kind":"session.started","text":"go"}`}},
		{Stream: stream, Type: typeEgress, Payload: egressPayload("8.8.8.8", "allowed", "public")},
		{Stream: stream, Type: typeEgress, Payload: egressPayload("10.0.0.1", "blocked", "private or reserved address")},
	}
	for _, a := range appends {
		a.Time = time.Unix(0, 0)
		if _, err := log.Append(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := History(ctx, log, stream)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(evs)
	if p.EgressAllowed != 1 || p.EgressBlocked != 1 {
		t.Fatalf("projected egress = %d allowed, %d blocked, want 1/1", p.EgressAllowed, p.EgressBlocked)
	}
}
