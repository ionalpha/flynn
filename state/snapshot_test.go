package state

import (
	"context"
	"fmt"
	"testing"
)

// populateState drives a provider through a representative set of mutations (sessions
// with turns, a skill upserted then deleted alongside a live one, memory written then
// deleted) so a snapshot has a non-trivial projection with tombstones to round-trip.
func populateState(t *testing.T, p Provider) {
	t.Helper()
	ctx := context.Background()
	for i := range 3 {
		s, err := p.Sessions().Create(ctx, Session{Title: fmt.Sprintf("s%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := p.Sessions().AppendTurn(ctx, Turn{SessionID: s.ID, Role: "user", Content: "hi"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	sk, err := p.Skills().Upsert(ctx, Skill{Slug: "deploy", Name: "Deploy", Body: "run it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Skills().Upsert(ctx, Skill{Slug: "keep", Name: "Keep", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Skills().Delete(ctx, sk.ID); err != nil {
		t.Fatal(err)
	}
	m, err := p.Memory().Write(ctx, MemoryItem{Kind: "fact", Content: "sky is blue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Memory().Write(ctx, MemoryItem{Kind: "fact", Content: "grass is green"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Memory().Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRoundTrip asserts a core's projection survives snapshot -> marshal ->
// unmarshal -> restore into a fresh core unchanged, so a snapshot is a faithful,
// deterministic capture of the read model, tombstones and slug index included.
func TestSnapshotRoundTrip(t *testing.T) {
	p := NewMemory().(*memProvider)
	populateState(t, p)

	p.core.mu.Lock()
	snap := p.core.snapshotLocked()
	p.core.mu.Unlock()

	payload, err := MarshalSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalSnapshot(payload)
	if err != nil {
		t.Fatal(err)
	}

	fresh := newCore(p.core.st, p.core.log)
	fresh.mu.Lock()
	fresh.restoreLocked(decoded)
	restored := fresh.snapshotLocked()
	fresh.mu.Unlock()

	back, err := MarshalSnapshot(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(back) {
		t.Fatalf("round-trip changed the projection:\n orig=%s\n back=%s", payload, back)
	}
	if snap.LastSeq == 0 {
		t.Fatal("expected a non-zero LastSeq after mutations")
	}
}
