package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spine/spinetest"
	"github.com/ionalpha/flynn/spine/sqlite"
)

// TestConformance holds the SQLite log to the identical contract as the
// in-memory one — one line proves byte-for-byte parity across backends.
func TestConformance(t *testing.T) {
	spinetest.RunSuite(t, func() spine.Log {
		l, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			panic(err)
		}
		return l
	})
}

// TestPersistsAcrossReopen is the point of a durable log: events written by one
// process are read back, in order, by the next.
func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "spine.db")

	l1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"start", "tool", "end"} {
		if _, err := l1.Append(ctx, spine.AppendInput{Stream: "run", Type: typ, Actor: spine.ActorAgent, Payload: map[string]any{"t": typ}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = l2.Close() }()

	got, err := l2.Read(ctx, spine.Query{Stream: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Type != "start" || got[2].Type != "end" {
		t.Fatalf("events did not survive reopen in order: %+v", got)
	}
	if got[1].Payload["t"] != "tool" {
		t.Fatalf("payload did not survive reopen: %+v", got[1].Payload)
	}
	// A further append continues the same Seq sequence, not restarts it.
	next, err := l2.Append(ctx, spine.AppendInput{Stream: "run", Type: "more", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq != 4 {
		t.Fatalf("post-reopen Seq = %d, want 4 (must continue, not restart)", next.Seq)
	}
}

// TestReopenRerunsMigrationsCleanly verifies the migration runner is idempotent
// against a populated database.
func TestReopenRerunsMigrationsCleanly(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "spine.db")
	for i := 0; i < 3; i++ {
		l, err := sqlite.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
	}
}

// TestAppendRejectsNonJSONPayload documents the durability contract: the spine
// is a serialization boundary, so a payload that cannot be encoded to JSON is
// rejected at Append rather than corrupting the log.
func TestAppendRejectsNonJSONPayload(t *testing.T) {
	ctx := context.Background()
	l, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	_, err = l.Append(ctx, spine.AppendInput{
		Stream: "s", Type: "e", Actor: spine.ActorAgent,
		Payload: map[string]any{"bad": make(chan int)}, // channels are not JSON-encodable
	})
	if err == nil {
		t.Fatal("Append accepted a non-JSON-encodable payload, want an error")
	}
	// And nothing was written.
	if got, _ := l.Read(ctx, spine.Query{Stream: "s"}); len(got) != 0 {
		t.Fatalf("a rejected append still wrote %d events", len(got))
	}
}

// TestClockOption verifies an unset Time is stamped from the injected clock.
func TestClockOption(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(4242, 0).UTC()
	l, err := sqlite.Open(ctx, ":memory:", sqlite.WithClock(clock.NewManual(at)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	e, err := l.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Time.Equal(at) {
		t.Fatalf("event Time = %v, want the clock's %v", e.Time, at)
	}
}
