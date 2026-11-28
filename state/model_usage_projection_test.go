package state

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/usage"
)

func projectionRecord(run, call, model string) usage.Record {
	n := usage.Count{N: 10, Basis: usage.Reported}
	return usage.Record{
		Run: run, Call: call, Provider: "p", Model: model,
		At:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Tokens: usage.Tokens{Input: n, Output: n, CacheRead: usage.Count{Basis: usage.Reported}, CacheWrite: usage.Count{Basis: usage.Reported}},
	}
}

func appendModelUsage(t *testing.T, log spine.Log, records ...usage.Record) {
	t.Helper()
	st := NewStamper("local", clock.System{}, hlc.NewClock(), ids.NewGenerator())
	in, err := st.RecordModelUsage(records)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func snapshotOf(t *testing.T, log spine.Log, p Provider) Snapshot {
	t.Helper()
	ctx := context.Background()
	if err := p.(*memProvider).Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	raw, found, err := log.LatestSnapshot(ctx, StateStream, 0)
	if err != nil || !found {
		t.Fatalf("LatestSnapshot = %v, %v; want a snapshot", found, err)
	}
	snap, err := UnmarshalSnapshot(raw.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestModelUsageSurvivesReplayAndSnapshot pins model usage records as ordinary
// event-sourced state: a snapshot that dropped them would look correct until the
// stream was long enough to checkpoint, and then a provider resuming from it would
// total less than the log says.
func TestModelUsageSurvivesReplayAndSnapshot(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	a, b := projectionRecord("r1", "c1", "m"), projectionRecord("r1", "c2", "m")
	appendModelUsage(t, log, a, b)

	replayed, err := Replay(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	want := []usage.Record{a, b}
	snap := snapshotOf(t, log, replayed)
	if !reflect.DeepEqual(snap.ModelUsageRecords, want) {
		t.Fatalf("snapshot after replay = %+v, want %+v", snap.ModelUsageRecords, want)
	}

	// The second replay resumes from that snapshot and must hold the same records.
	resumed, err := Replay(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotOf(t, log, resumed).ModelUsageRecords; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot after resume = %+v, want %+v", got, want)
	}
}

// TestModelUsageFirstRecordUnderAKeyStands pins the fold for a log that carries two
// different records under one key, which a store that refuses a conflict never
// writes but a hand-built or merged log can contain: the earlier one stands, so a
// replay never rewrites what was already counted.
func TestModelUsageFirstRecordUnderAKeyStands(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	first, second := projectionRecord("r1", "c1", "first"), projectionRecord("r1", "c1", "second")
	appendModelUsage(t, log, first)
	appendModelUsage(t, log, second)

	p, err := Replay(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotOf(t, log, p).ModelUsageRecords; !reflect.DeepEqual(got, []usage.Record{first}) {
		t.Fatalf("records = %+v, want only the first", got)
	}
}

// TestModelUsageEventsFailClosed pins that a malformed model usage event stops the
// replay with an error instead of folding to an empty set, which would let a log
// that lost its records read as one that never had any.
func TestModelUsageEventsFailClosed(t *testing.T) {
	st := NewStamper("local", clock.System{}, hlc.NewClock(), ids.NewGenerator())
	empty, err := st.RecordModelUsage(nil)
	if err != nil {
		t.Fatal(err)
	}
	garbled, err := st.input(EvModelUsageRecorded, map[string]any{keyModelUsage: "not a list"})
	if err != nil {
		t.Fatal(err)
	}

	for name, in := range map[string]spine.AppendInput{"no records": empty, "undecodable": garbled} {
		log := spine.NewMemoryLog()
		if _, err := log.Append(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := Replay(context.Background(), log); err == nil {
			t.Errorf("%s: Replay succeeded, want an error", name)
		}
	}
}

// TestDecodeModelUsageRecordsRefusesAnEmptyBatch: a durable backend decodes the
// payload to project it, and an event with no records would project nothing while
// looking recorded.
func TestDecodeModelUsageRecordsRefusesAnEmptyBatch(t *testing.T) {
	if _, err := DecodeModelUsageRecords(map[string]any{keyModelUsage: []any{}}); err == nil {
		t.Fatal("DecodeModelUsageRecords accepted an empty batch, want an error")
	}
	if _, err := DecodeModelUsageRecords(map[string]any{}); err == nil {
		t.Fatal("DecodeModelUsageRecords accepted a payload with no records, want an error")
	}
}
