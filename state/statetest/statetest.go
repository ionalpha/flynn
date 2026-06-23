// Package statetest is the conformance suite for state.Provider. Every backend
// (the in-memory default, SQLite, a host's Postgres) runs RunSuite and must
// behave identically, so the durable providers are held to byte-for-byte the
// same contract as the reference in-memory one rather than re-tested by hand.
package statetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// RunSuite runs the full state.Provider contract against providers built by
// newProvider. Each subtest gets a fresh provider.
func RunSuite(t *testing.T, newProvider func() state.Provider) {
	t.Helper()
	t.Run("NameAndClose", func(t *testing.T) {
		p := newProvider()
		if p.Name() == "" {
			t.Error("Name() is empty")
		}
		if err := p.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	t.Run("Sessions", func(t *testing.T) { testSessions(t, newProvider()) })
	t.Run("Skills", func(t *testing.T) { testSkills(t, newProvider()) })
	t.Run("SkillOptimisticConcurrency", func(t *testing.T) { testSkillCAS(t, newProvider()) })
	t.Run("SkillTombstone", func(t *testing.T) { testSkillTombstone(t, newProvider()) })
	t.Run("Memory", func(t *testing.T) { testMemory(t, newProvider()) })
	t.Run("Envelope", func(t *testing.T) { testEnvelope(t, newProvider()) })
	t.Run("Concurrency", func(t *testing.T) { testConcurrency(t, newProvider()) })
	t.Run("EventSourced", func(t *testing.T) { testEventSourced(t, newProvider()) })
}

// eventLogged is the optional capability of a provider that exposes the spine its
// state mutations are recorded on. Every event-sourced backend implements it; the
// invariant check below holds such a backend to "no write bypasses the log".
type eventLogged interface {
	Log() spine.Log
}

// testEventSourced enforces the architecture's central invariant: every state
// mutation appends to the event log, and the log alone fully describes the state.
// A backend that writes directly to its tables (the bug this guards against) would
// produce a short or empty stream and fail here, and the suite proves it for every
// backend rather than re-testing each by hand. Providers with no event log (a host
// may back state differently) are skipped.
func testEventSourced(t *testing.T, p state.Provider) {
	el, ok := p.(eventLogged)
	if !ok {
		t.Skip("provider does not expose an event log")
	}
	ctx := context.Background()

	s, err := p.Sessions().Create(ctx, state.Session{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "k", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Skills().Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	m, err := p.Memory().Write(ctx, state.MemoryItem{Content: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Memory().Delete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Sessions().Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	events, err := el.Log().Read(ctx, spine.Query{Stream: state.StateStream})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		state.EvSessionCreated: 1, state.EvTurnAppended: 1, state.EvSessionDeleted: 1,
		state.EvSkillUpserted: 1, state.EvSkillDeleted: 1,
		state.EvMemoryWritten: 1, state.EvMemoryDeleted: 1,
	}
	got := map[string]int{}
	for _, e := range events {
		got[e.Type]++
	}
	for typ, n := range want {
		if got[typ] != n {
			t.Errorf("event %q recorded %d times, want %d (a write bypassed the log)", typ, got[typ], n)
		}
	}
	if len(events) != len(want) {
		t.Errorf("state stream has %d events, want %d (one per mutation)", len(events), len(want))
	}

	// The log is authoritative: a provider folded purely from it reproduces the
	// live reads, so state is genuinely a projection of the spine.
	replayed, err := state.Replay(ctx, el.Log())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := liveSnapshot(t, replayed), liveSnapshot(t, p); got != want {
		t.Fatalf("state replayed from the log differs from live reads\n replay: %s\n live:   %s", got, want)
	}
}

// liveSnapshot serialises a provider's observable live state to a stable JSON
// string so two providers can be compared for byte-for-byte equivalence.
func liveSnapshot(t *testing.T, p state.Provider) string {
	t.Helper()
	ctx := context.Background()
	var view struct {
		Sessions []state.Session
		Skills   []state.Skill
		Memory   []state.MemoryItem
	}
	var err error
	if view.Sessions, err = p.Sessions().List(ctx); err != nil {
		t.Fatal(err)
	}
	if view.Skills, err = p.Skills().Search(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	if view.Memory, err = p.Memory().Recall(ctx, state.RecallQuery{}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testSessions(t *testing.T, p state.Provider) {
	ctx := context.Background()
	ss := p.Sessions()

	if _, err := ss.Get(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}

	s, err := ss.Create(ctx, state.Session{Title: "t", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Fatal("Create did not assign an ID")
	}
	if got, err := ss.Get(ctx, s.ID); err != nil || got.ID != s.ID {
		t.Fatalf("Get = (%q, %v)", got.ID, err)
	}

	for i := 0; i < 3; i++ {
		tn, err := ss.AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user", Content: fmt.Sprintf("c%d", i)})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if tn.Seq != int64(i+1) {
			t.Fatalf("turn %d Seq = %d, want %d", i, tn.Seq, i+1)
		}
	}
	turns, err := ss.Turns(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[0].Content != "c0" || turns[2].Content != "c2" {
		t.Fatalf("turns not resumed in order: %+v", turns)
	}
	if _, err := ss.AppendTurn(ctx, state.Turn{SessionID: "missing"}); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("AppendTurn(missing) = %v, want ErrNotFound", err)
	}

	if _, err := ss.Create(ctx, state.Session{Title: "t2"}); err != nil {
		t.Fatal(err)
	}
	list, err := ss.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d, want 2", len(list))
	}
	if list[0].CreatedAt.After(list[1].CreatedAt) {
		t.Fatal("List is not oldest-first")
	}

	if err := ss.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Get(ctx, s.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("deleted session is still readable")
	}
	if l, _ := ss.List(ctx); len(l) != 1 {
		t.Fatalf("List after delete = %d, want 1", len(l))
	}
	if err := ss.Delete(ctx, s.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double Delete = %v, want ErrNotFound", err)
	}
}

func testSkills(t *testing.T, p state.Provider) {
	ctx := context.Background()
	sk := p.Skills()

	if _, err := sk.Get(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}

	a, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship it", Tags: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Version != 1 {
		t.Fatalf("create = (id %q, version %d), want id + version 1", a.ID, a.Version)
	}
	byID, err := sk.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	bySlug, err := sk.Get(ctx, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != bySlug.ID || byID.Body != "ship it" {
		t.Fatalf("get-by-id/slug disagree or wrong body: %+v / %+v", byID, bySlug)
	}

	// Update preserves CreatedAt and bumps the content version.
	b, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship faster"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Version != 2 || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("update = (id %q, v %d, created %v), want same id, v2, created %v", b.ID, b.Version, b.CreatedAt, a.CreatedAt)
	}

	// Scope isolation.
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Scope: state.Scope{Project: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	scoped, err := sk.List(ctx, state.Scope{Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Scope.Project != "alpha" {
		t.Fatalf("scoped List = %+v, want only the alpha skill", scoped)
	}

	// Search + limit.
	hits, err := sk.Search(ctx, "faster", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Slug != "deploy" {
		t.Fatalf("Search(faster) = %+v, want the global deploy skill", hits)
	}
	if capped, _ := sk.Search(ctx, "", 1); len(capped) > 1 {
		t.Fatalf("Search limit 1 returned %d", len(capped))
	}
}

func testSkillCAS(t *testing.T, p state.Provider) {
	ctx := context.Background()
	sk := p.Skills()

	created, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.SyncVersion != 1 {
		t.Fatalf("create SyncVersion = %d, want 1", created.SyncVersion)
	}

	upd := created
	upd.Body = "v2"
	saved, err := sk.Upsert(ctx, upd)
	if err != nil {
		t.Fatalf("matching-version update: %v", err)
	}
	if saved.SyncVersion != 2 {
		t.Fatalf("update SyncVersion = %d, want 2", saved.SyncVersion)
	}

	upd.Body = "v3" // upd still carries the now-stale SyncVersion 1
	if _, err := sk.Upsert(ctx, upd); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "v4"}); err != nil {
		t.Fatalf("unconditional (zero-version) update: %v", err)
	}
	ghost := state.Skill{Slug: "ghost"}
	ghost.SyncVersion = 5
	if _, err := sk.Upsert(ctx, ghost); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("create-with-version = %v, want ErrConflict", err)
	}
}

func testSkillTombstone(t *testing.T, p state.Provider) {
	ctx := context.Background()
	sk := p.Skills()

	original, _ := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship"})
	if err := sk.Delete(ctx, "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Get(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if l, _ := sk.List(ctx, state.Scope{}); len(l) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(l))
	}
	if h, _ := sk.Search(ctx, "ship", 0); len(h) != 0 {
		t.Fatalf("Search after delete = %d, want 0", len(h))
	}
	if err := sk.Delete(ctx, "deploy"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}

	revived, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Body: "back"})
	if err != nil {
		t.Fatal(err)
	}
	if revived.Deleted {
		t.Fatal("upsert over a tombstone must resurrect it")
	}
	if !original.UpdatedHLC.Before(revived.UpdatedHLC) {
		t.Fatal("resurrection must carry a newer HLC")
	}
	if got, err := sk.Get(ctx, "deploy"); err != nil || got.Body != "back" {
		t.Fatalf("resurrected = (%q, %v)", got.Body, err)
	}
}

func testMemory(t *testing.T, p state.Provider) {
	ctx := context.Background()
	mem := p.Memory()

	a, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "the user prefers Go"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("Write did not assign an ID")
	}
	if _, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "deploys go to Cloudflare", Scope: state.Scope{Project: "x"}}); err != nil {
		t.Fatal(err)
	}

	hits, err := mem.Recall(ctx, state.RecallQuery{Query: "prefers", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Content != "the user prefers Go" {
		t.Fatalf("Recall = %+v, want the single matching fact", hits)
	}
	// Scope narrows.
	if scoped, _ := mem.Recall(ctx, state.RecallQuery{Query: "deploys", Scope: state.Scope{Project: "x"}}); len(scoped) != 1 {
		t.Fatalf("scoped Recall = %d, want 1", len(scoped))
	}

	if err := mem.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Recall(ctx, state.RecallQuery{Query: "prefers"}); len(got) != 0 {
		t.Fatalf("Recall after delete = %d, want 0", len(got))
	}
	if err := mem.Delete(ctx, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

func testEnvelope(t *testing.T, p state.Provider) {
	ctx := context.Background()

	sk := p.Skills()
	a, _ := sk.Upsert(ctx, state.Skill{Slug: "a"})
	if a.SyncVersion != 1 || a.OriginInstanceID == "" || a.LastWriterID == "" || a.UpdatedHLC.IsZero() {
		t.Fatalf("create envelope incomplete: %+v", a.Envelope)
	}
	if a.OriginInstanceID != a.LastWriterID {
		t.Fatalf("on create, origin %q should equal last-writer %q", a.OriginInstanceID, a.LastWriterID)
	}

	upd := a
	upd.Body = "v2"
	b, _ := sk.Upsert(ctx, upd)
	if b.SyncVersion != 2 || !a.UpdatedHLC.Before(b.UpdatedHLC) {
		t.Fatalf("update did not advance envelope: v=%d hlc %v->%v", b.SyncVersion, a.UpdatedHLC, b.UpdatedHLC)
	}
	if b.OriginInstanceID != a.OriginInstanceID {
		t.Fatal("OriginInstanceID must be preserved across updates")
	}

	// Appending a turn advances the session's envelope.
	s, _ := p.Sessions().Create(ctx, state.Session{})
	turn, _ := p.Sessions().AppendTurn(ctx, state.Turn{SessionID: s.ID, Role: "user"})
	if turn.UpdatedHLC.IsZero() || turn.LastWriterID == "" {
		t.Fatalf("turn envelope not stamped: %+v", turn.Envelope)
	}
	got, _ := p.Sessions().Get(ctx, s.ID)
	if !s.UpdatedHLC.Before(got.UpdatedHLC) {
		t.Fatal("session HLC did not advance on append")
	}
}

func testConcurrency(t *testing.T, p state.Provider) {
	ctx := context.Background()
	sk := p.Skills()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = sk.Upsert(ctx, state.Skill{Slug: fmt.Sprintf("s%d", i%10), Name: "n", Body: "b"})
			_, _ = sk.Search(ctx, "n", 0)
			_, _ = sk.List(ctx, state.Scope{})
		}(i)
	}
	wg.Wait()

	all, err := sk.List(ctx, state.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Fatalf("got %d skills, want 10 distinct slugs", len(all))
	}
}
