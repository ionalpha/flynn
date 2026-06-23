// Package spinetest is the conformance suite for spine.Log. Every backend (the
// in-memory default, SQLite, a host's store) runs RunSuite and must behave
// identically, so durable logs are held to byte-for-byte the same ordering and
// immutability contract as the reference MemoryLog rather than re-tested by hand.
package spinetest

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// RunSuite runs the full spine.Log contract against logs built by newLog. Each
// subtest gets a fresh log.
func RunSuite(t *testing.T, newLog func() spine.Log) {
	t.Helper()
	t.Run("PerStreamMonotonicSeq", func(t *testing.T) { testMonotonic(t, newLog()) })
	t.Run("StreamsIndependent", func(t *testing.T) { testIndependent(t, newLog()) })
	t.Run("ReadAfterAndLimit", func(t *testing.T) { testReadPaging(t, newLog()) })
	t.Run("Time", func(t *testing.T) { testTime(t, newLog()) })
	t.Run("PayloadImmutableAndPreserved", func(t *testing.T) { testPayload(t, newLog()) })
	t.Run("EmptyStream", func(t *testing.T) { testEmpty(t, newLog()) })
	t.Run("Concurrency", func(t *testing.T) { testConcurrency(t, newLog()) })
}

func testMonotonic(t *testing.T, log spine.Log) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e, err := log.Append(ctx, spine.AppendInput{Stream: "run", Type: "tick", Actor: spine.ActorAgent})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if want := int64(i + 1); e.Seq != want {
			t.Fatalf("event %d Seq = %d, want %d", i, e.Seq, want)
		}
	}
}

func testIndependent(t *testing.T, log spine.Log) {
	ctx := context.Background()
	if _, err := log.Append(ctx, spine.AppendInput{Stream: "a", Type: "e", Actor: spine.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, spine.AppendInput{Stream: "a", Type: "e", Actor: spine.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	b, err := log.Append(ctx, spine.AppendInput{Stream: "b", Type: "e", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if b.Seq != 1 {
		t.Fatalf("stream b first Seq = %d, want 1 (streams are independent)", b.Seq)
	}
}

func testReadPaging(t *testing.T, log spine.Log) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorSystem}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("read all = %d, want 5", len(all))
	}
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Fatalf("read[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}
	// AfterSeq is an exclusive lower bound; Limit caps the result.
	page, err := log.Read(ctx, spine.Query{Stream: "s", AfterSeq: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Seq != 3 || page[1].Seq != 4 {
		t.Fatalf("page = %+v, want Seq 3,4", page)
	}
	// AfterSeq past the end returns nothing.
	if tail, _ := log.Read(ctx, spine.Query{Stream: "s", AfterSeq: 99}); len(tail) != 0 {
		t.Fatalf("read past end = %d, want 0", len(tail))
	}
}

func testTime(t *testing.T, log spine.Log) {
	ctx := context.Background()
	// A zero input Time is stamped by the log's clock.
	e, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if e.Time.IsZero() {
		t.Fatal("zero input Time was not stamped")
	}
	// An explicit Time is preserved exactly (to the nanosecond) across a round-trip.
	at := time.Date(2026, 6, 23, 11, 0, 0, 123456789, time.UTC)
	if _, err := log.Append(ctx, spine.AppendInput{Stream: "s", Type: "e", Actor: spine.ActorAgent, Time: at}); err != nil {
		t.Fatal(err)
	}
	got, err := log.Read(ctx, spine.Query{Stream: "s", AfterSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Time.Equal(at) {
		t.Fatalf("stored Time = %v, want %v", got[0].Time, at)
	}
}

func testPayload(t *testing.T, log spine.Log) {
	ctx := context.Background()
	in := map[string]any{"k": "v", "nested": map[string]any{"a": "b"}}
	if _, err := log.Append(ctx, spine.AppendInput{
		Stream: "s", Type: "e", Actor: spine.ActorHuman, Payload: in,
		TraceID: "tr", SpanID: "sp", CausationID: "cz", OriginInstanceID: "node-1",
	}); err != nil {
		t.Fatal(err)
	}
	in["k"] = "mutated" // a caller mutating its map after Append must not change history

	got, err := log.Read(ctx, spine.Query{Stream: "s"})
	if err != nil {
		t.Fatal(err)
	}
	e := got[0]
	if !jsonEqual(e.Payload, map[string]any{"k": "v", "nested": map[string]any{"a": "b"}}) {
		t.Fatalf("payload = %v, want the original (log must be immutable)", e.Payload)
	}
	if e.Type != "e" || e.Actor != spine.ActorHuman {
		t.Fatalf("type/actor not preserved: %q/%q", e.Type, e.Actor)
	}
	if e.TraceID != "tr" || e.SpanID != "sp" || e.CausationID != "cz" || e.OriginInstanceID != "node-1" {
		t.Fatalf("linkage fields not preserved: %+v", e)
	}
}

func testEmpty(t *testing.T, log spine.Log) {
	ctx := context.Background()
	got, err := log.Read(ctx, spine.Query{Stream: "never-written"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty stream read = %d, want 0", len(got))
	}
}

func testConcurrency(t *testing.T, log spine.Log) {
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = log.Append(ctx, spine.AppendInput{Stream: streamName(i % 10), Type: "e", Actor: spine.ActorAgent})
		}(i)
	}
	wg.Wait()

	for s := 0; s < 10; s++ {
		got, err := log.Read(ctx, spine.Query{Stream: streamName(s)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 5 {
			t.Fatalf("stream %d has %d events, want 5", s, len(got))
		}
		for i, e := range got {
			if e.Seq != int64(i+1) {
				t.Fatalf("stream %d not dense: event[%d].Seq = %d", s, i, e.Seq)
			}
		}
	}
}

func streamName(i int) string { return "s" + strconv.Itoa(i) }

// jsonEqual compares two payloads by canonical JSON, so backends that round-trip
// through JSON (e.g. SQLite, where int becomes float64) still match the
// in-memory reference. json.Marshal sorts map keys, giving a stable encoding.
func jsonEqual(a, b map[string]any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
