package main

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// TestAppendProvenanceRoundTrip proves the emit side writes exactly what the verify
// side reads: appendProvenance records a declaration onto a run's stream that
// chain.ProvenanceOf recovers from the same stream, so `flynn spine verify` surfaces an
// external run's tier mix from its sealed record.
func TestAppendProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/provenance"
	declared := externalProvenance{
		harness:    "codex",
		attested:   3,
		nativeRate: 0.25,
		drift:      map[string]int{"no-native-tools": 2},
	}
	if err := appendProvenance(ctx, store.Log(), stream, declared); err != nil {
		t.Fatalf("appendProvenance: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	p, ok := chain.ProvenanceOf(events)
	if !ok {
		t.Fatal("appended declaration was not readable back from the stream")
	}
	if p.Harness != "codex" || p.Effects != chain.TierEnforced || p.Reasoning != chain.TierUnobserved || p.Replayable {
		t.Fatalf("round-tripped provenance wrong: %+v", p)
	}
	// Through the real store, so the tally, the rate, and the drift map are read back off
	// persisted bytes rather than the in-memory values the producer wrote. Each arrives
	// through JSON, where an int becomes a float64 and a map becomes map[string]any.
	if p.AttestedEvents != 3 {
		t.Fatalf("attested events = %d, want 3", p.AttestedEvents)
	}
	if p.NativeToolRate != 0.25 {
		t.Fatalf("native tool rate = %v, want 0.25", p.NativeToolRate)
	}
	if p.Drift["no-native-tools"] != 2 {
		t.Fatalf("drift = %v, want no-native-tools x2", p.Drift)
	}
}

// TestAppendProvenanceOmitsEmptyDrift proves a run whose harness honored the contract
// records no drift key at all, rather than an empty map a reader might puzzle over.
func TestAppendProvenanceOmitsEmptyDrift(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const stream = "run/clean"
	if err := appendProvenance(ctx, store.Log(), stream, externalProvenance{harness: "codex"}); err != nil {
		t.Fatalf("appendProvenance: %v", err)
	}
	events, err := store.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := chain.ProvenanceOf(events)
	if !ok {
		t.Fatal("declaration not readable")
	}
	if len(p.Drift) != 0 {
		t.Errorf("a compliant run must record no drift, got %v", p.Drift)
	}
	if _, present := events[0].Payload[chain.ProvenanceDriftKey]; present {
		t.Error("the drift key must be absent, not an empty map")
	}
}
