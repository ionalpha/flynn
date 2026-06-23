package state_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/state"
)

func TestSessionsAppendAndResume(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()

	s, err := p.Sessions().Create(ctx, state.Session{Title: "first", Model: "anthropic:claude-opus-4-8"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.ID == "" {
		t.Fatal("expected an assigned session ID")
	}

	for i, content := range []string{"hello", "world", "again"} {
		got, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: content})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if want := int64(i + 1); got.Seq != want {
			t.Fatalf("turn %d: Seq = %d, want %d", i, got.Seq, want)
		}
	}

	// Resuming a session means reading its turns back in order.
	turns, err := p.Sessions().Turns(ctx, s.ID)
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}
	if turns[0].Content != "hello" || turns[2].Content != "again" {
		t.Fatalf("turns out of order: %q .. %q", turns[0].Content, turns[2].Content)
	}
}

func TestAppendTurnUnknownSession(t *testing.T) {
	p := state.NewMemory()
	_, err := p.Sessions().AppendTurn(context.Background(), state.Turn{SessionID: "nope"})
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSkillsUpsertVersionAndSearch(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	sk := p.Skills()

	first, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship the binary", Tags: []string{"ops"}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("first version = %d, want 1", first.Version)
	}

	// Same (scope, slug) updates in place and bumps the version, keeping CreatedAt.
	second, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship faster"})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert created a new id %q (was %q)", second.ID, first.ID)
	}
	if second.Version != 2 {
		t.Fatalf("second version = %d, want 2", second.Version)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("CreatedAt should be preserved across upsert")
	}

	hits, err := sk.Search(ctx, "faster", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Slug != "deploy" {
		t.Fatalf("search returned %d hits, want the deploy skill", len(hits))
	}
	if none, _ := sk.Search(ctx, "nonexistent-term", 0); len(none) != 0 {
		t.Fatalf("expected no hits, got %d", len(none))
	}
}

func TestSkillsScopedList(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	sk := p.Skills()
	proj := state.Scope{Project: "alpha"}

	if _, err := sk.Upsert(ctx, state.Skill{Slug: "g", Name: "global"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "p", Name: "scoped", Scope: proj}); err != nil {
		t.Fatal(err)
	}

	scoped, err := sk.List(ctx, proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Slug != "p" {
		t.Fatalf("scoped list = %+v, want only the project-scoped skill", scoped)
	}
}

func TestMemoryWriteAndRecall(t *testing.T) {
	ctx := context.Background()
	p := state.NewMemory()
	mem := p.Memory()

	if _, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "the user prefers Go"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "deploys go to Cloudflare"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	hits, err := mem.Recall(ctx, state.RecallQuery{Query: "prefers", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 1 || hits[0].Content != "the user prefers Go" {
		t.Fatalf("recall = %+v, want the single matching fact", hits)
	}
}
