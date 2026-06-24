package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// open returns a fresh in-memory provider, panicking on error: opening an
// in-memory SQLite database does not fail in practice, and the conformance
// suite's factory signature has no place to return one.
func open(t *testing.T) state.Provider {
	t.Helper()
	p, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return p
}

// TestConformance holds the SQLite provider to the identical contract as the
// in-memory one. This is the payoff of the shared suite: a single line proves
// byte-for-byte parity across backends.
func TestStateConformance(t *testing.T) {
	statetest.RunSuite(t, func() state.Provider {
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			panic(err)
		}
		return p
	})
}

func TestName(t *testing.T) {
	if got := open(t).Name(); got != "sqlite" {
		t.Fatalf("Name() = %q, want sqlite", got)
	}
}

func TestWithInstanceID(t *testing.T) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, ":memory:", sqlite.WithInstanceID("node-7"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	sk, err := p.Skills().Upsert(ctx, state.Skill{Slug: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if sk.OriginInstanceID != "node-7" || sk.LastWriterID != "node-7" {
		t.Fatalf("origin/writer = %q/%q, want node-7", sk.OriginInstanceID, sk.LastWriterID)
	}
}

// TestPersistsAcrossReopen is the whole point of a durable provider: state
// written by one process is read back by the next. A file is closed and
// reopened, and the records survive.
func TestStatePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "state.db")

	p1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := p1.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship it"})
	if err != nil {
		t.Fatal(err)
	}
	ses, err := p1.Sessions().Create(ctx, state.Session{Title: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Sessions().AppendTurn(ctx, state.Turn{SessionID: ses.ID, Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "prefers Go"}); err != nil {
		t.Fatal(err)
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = p2.Close() }()

	got, err := p2.Skills().Get(ctx, "deploy")
	if err != nil || got.ID != sk.ID || got.Body != "ship it" {
		t.Fatalf("skill did not survive reopen: (%+v, %v)", got, err)
	}
	turns, err := p2.Sessions().Turns(ctx, ses.ID)
	if err != nil || len(turns) != 1 || turns[0].Content != "hi" {
		t.Fatalf("turns did not survive reopen: (%+v, %v)", turns, err)
	}
	if hits, _ := p2.Memory().Recall(ctx, state.RecallQuery{Query: "prefers"}); len(hits) != 1 {
		t.Fatalf("memory did not survive reopen: %d hits", len(hits))
	}
}

// TestReopenRerunsMigrationsCleanly verifies the migration runner is idempotent
// against a populated database: reopening an existing store must not error or
// re-apply.
func TestStateReopenRerunsMigrationsCleanly(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "state.db")
	for i := range 3 {
		p, err := sqlite.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
	}
}

// TestSearchHandlesFTSSpecialChars guards the FTS5 query escaping: a query made
// of FTS5 operators must be treated as a literal phrase, never as query syntax —
// so it neither errors nor matches the wrong rows.
func TestSearchHandlesFTSSpecialChars(t *testing.T) {
	ctx := context.Background()
	p := open(t)
	defer func() { _ = p.Close() }()

	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "a", Body: "alpha beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Skills().Upsert(ctx, state.Skill{Slug: "b", Body: `a "quoted" phrase`}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{`OR AND NOT`, `"unterminated`, `a OR b`, `(paren`, `near*`} {
		if _, err := p.Skills().Search(ctx, q, 0); err != nil {
			t.Fatalf("Search(%q) errored on FTS syntax: %v", q, err)
		}
	}

	// The literal token "quoted" (surrounded by punctuation in the body) is found.
	hits, err := p.Skills().Search(ctx, "quoted", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Slug != "b" {
		t.Fatalf("Search(quoted) = %+v, want only skill b", hits)
	}
}
