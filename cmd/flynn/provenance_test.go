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
	if err := appendProvenance(ctx, store.Log(), stream, "codex", 3); err != nil {
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
	// Through the real store, so the attested tally is read back off persisted bytes
	// rather than the in-memory map the producer wrote.
	if p.AttestedEvents != 3 {
		t.Fatalf("attested events = %d, want 3", p.AttestedEvents)
	}
}
