package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

func TestEnvelopeStampedOnCreate(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()

	ses, err := p.Sessions().Create(ctx, state.Session{Title: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if ses.SyncVersion != 1 || ses.OriginInstanceID != "local" {
		t.Fatalf("session envelope = {v:%d, origin:%q}, want {1, local}", ses.SyncVersion, ses.OriginInstanceID)
	}

	sk, err := p.Skills().Upsert(ctx, state.Skill{Slug: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if sk.SyncVersion != 1 || sk.OriginInstanceID != "local" {
		t.Fatalf("skill envelope = {v:%d, origin:%q}, want {1, local}", sk.SyncVersion, sk.OriginInstanceID)
	}

	it, err := p.Memory().Write(ctx, state.MemoryItem{Content: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if it.SyncVersion != 1 || it.OriginInstanceID != "local" {
		t.Fatalf("memory envelope = {v:%d, origin:%q}, want {1, local}", it.SyncVersion, it.OriginInstanceID)
	}
}

func TestWithInstanceID(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory(state.WithInstanceID("node-7"))
	sk, err := p.Skills().Upsert(ctx, state.Skill{Slug: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if sk.OriginInstanceID != "node-7" {
		t.Fatalf("origin = %q, want node-7", sk.OriginInstanceID)
	}
}

func TestSessionSyncVersionBumpsOnAppend(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	s, err := p.Sessions().Create(ctx, state.Session{})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.SyncVersion != 1 || turn.OriginInstanceID != "local" {
		t.Fatalf("turn envelope = {v:%d, origin:%q}, want {1, local}", turn.SyncVersion, turn.OriginInstanceID)
	}
	got, err := p.Sessions().Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncVersion != 2 { // 1 on create, +1 per appended turn
		t.Fatalf("session SyncVersion = %d after one append, want 2", got.SyncVersion)
	}
}

func TestSkillOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()

	created, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.SyncVersion != 1 {
		t.Fatalf("create SyncVersion = %d, want 1", created.SyncVersion)
	}

	// Update carrying the version we read → succeeds and bumps to 2.
	upd := created
	upd.Body = "v2"
	saved, err := p.Skills().Upsert(ctx, upd)
	if err != nil {
		t.Fatalf("matching-version update: %v", err)
	}
	if saved.SyncVersion != 2 {
		t.Fatalf("update SyncVersion = %d, want 2", saved.SyncVersion)
	}

	// Re-using the now-stale version → ErrConflict.
	upd.Body = "v3"
	if _, err := p.Skills().Upsert(ctx, upd); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}

	// A zero SyncVersion writes unconditionally.
	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "v4"}); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}

	// Creating with a non-zero version expected an existing record → ErrConflict.
	ghost := state.Skill{Slug: "ghost"}
	ghost.SyncVersion = 5
	if _, err := p.Skills().Upsert(ctx, ghost); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("create-with-version = %v, want ErrConflict", err)
	}
}

func TestSkillOriginPreservedOnUpdate(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory(state.WithInstanceID("node-1"))

	created, err := p.Skills().Upsert(ctx, state.Skill{Slug: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// A foreign update must not be able to rewrite the origin.
	upd := created
	upd.OriginInstanceID = "node-2"
	upd.Body = "changed"
	saved, err := p.Skills().Upsert(ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OriginInstanceID != "node-1" {
		t.Fatalf("origin = %q after update, want node-1 (preserved)", saved.OriginInstanceID)
	}
}

func TestUpdatedHLCMonotonicAndLastWriter(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory(state.WithInstanceID("node-1"))

	a, err := p.Skills().Upsert(ctx, state.Skill{Slug: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Skills().Upsert(ctx, state.Skill{Slug: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.UpdatedHLC.IsZero() || !a.UpdatedHLC.Before(b.UpdatedHLC) {
		t.Fatalf("UpdatedHLC not stamped/monotonic: %v then %v", a.UpdatedHLC, b.UpdatedHLC)
	}
	if a.LastWriterID != "node-1" || b.LastWriterID != "node-1" {
		t.Fatalf("LastWriterID = %q/%q, want node-1", a.LastWriterID, b.LastWriterID)
	}

	// An update advances the HLC and re-stamps the writer.
	upd := a
	upd.Body = "v2"
	saved, err := p.Skills().Upsert(ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	if !b.UpdatedHLC.Before(saved.UpdatedHLC) {
		t.Fatalf("update HLC not after prior write: %v then %v", b.UpdatedHLC, saved.UpdatedHLC)
	}

	// Appending a turn advances the session's HLC.
	s, _ := p.Sessions().Create(ctx, state.Session{})
	turn, _ := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user"})
	if turn.UpdatedHLC.IsZero() || turn.LastWriterID != "node-1" {
		t.Fatalf("turn envelope not stamped: hlc=%v writer=%q", turn.UpdatedHLC, turn.LastWriterID)
	}
	got, _ := p.Sessions().Get(ctx, s.ID)
	if !s.UpdatedHLC.Before(got.UpdatedHLC) {
		t.Fatalf("session HLC did not advance on append: %v then %v", s.UpdatedHLC, got.UpdatedHLC)
	}
}
