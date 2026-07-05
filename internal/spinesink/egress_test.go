package spinesink_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/spine"
)

// TestEgressSinkRecordsDecisions wires netguard's observer to the spine and asserts an
// allow and a block decision both land on the run's stream as net.egress events,
// carrying the host, verdict, and reason.
func TestEgressSinkRecordsDecisions(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewEgress(log, "run-1")

	sink.Observe(netguard.Decision{Host: "203.0.113.7", Allowed: true, Reason: "public"})
	sink.Observe(netguard.Decision{Host: "10.0.0.1", Allowed: false, Reason: "private or reserved address"})

	events, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want two egress decisions", len(events))
	}
	for _, e := range events {
		if e.Type != "net.egress" {
			t.Fatalf("event type = %q, want net.egress", e.Type)
		}
		if e.Actor != spine.ActorAgent {
			t.Fatalf("actor = %q, want agent", e.Actor)
		}
	}
	if got := events[0].Payload["verdict"]; got != "allowed" {
		t.Fatalf("first verdict = %v, want allowed", got)
	}
	if got := events[0].Payload["host"]; got != "203.0.113.7" {
		t.Fatalf("first host = %v, want 203.0.113.7", got)
	}
	if got := events[1].Payload["verdict"]; got != "blocked" {
		t.Fatalf("second verdict = %v, want blocked", got)
	}
	if got := events[1].Payload["reason"]; got != "private or reserved address" {
		t.Fatalf("second reason = %v, want the block reason", got)
	}
}

// TestEgressSinkIsNetguardObserver pins that an EgressSink's Observe method satisfies
// the netguard.Observer type, so it can be seeded on a run's context directly.
func TestEgressSinkIsNetguardObserver(_ *testing.T) {
	var _ netguard.Observer = spinesink.NewEgress(spine.NewMemoryLog(), "run-1").Observe
}
