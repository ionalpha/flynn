package sqlite

// The projection and payload paths. Two invariants are gated here. First, the log is
// authoritative: a rebuild folds the stream through the same projection the live write
// path uses, so an event it cannot decode, cannot recognise, or cannot project must stop
// the rebuild rather than leave the tables half-reconciled. Second, a snapshot is a
// derived cache: one that fails to verify or to decode is skipped and the whole stream is
// folded instead, which is slower but never wrong.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// appendRaw puts one event on a stream with its payload bytes verbatim, so a test can
// place an event the projection cannot decode onto an otherwise healthy log.
func appendRaw(t *testing.T, s *Store, stream, typ, payload string) {
	t.Helper()
	if _, err := s.Log().Append(context.Background(), spine.AppendInput{
		Stream: stream, Type: typ, Actor: spine.ActorSystem, Time: testAt,
		RawPayload: json.RawMessage(payload),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRebuildRejectsUnprojectableStateEvents is the fold's fail-closed gate: an event on
// the state stream whose post-image does not decode, or whose type the projection does not
// know, aborts the rebuild. Skipping it would silently produce tables that no longer match
// the log while still reporting success, which is exactly the drift the event-sourced
// projection exists to make impossible.
func TestRebuildRejectsUnprojectableStateEvents(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		typ     string
		payload string
	}{
		{"session with no post-image", state.EvSessionCreated, `{}`},
		{"tombstoned session with no post-image", state.EvSessionDeleted, `{}`},
		{"turn with no post-image", state.EvTurnAppended, `{}`},
		// The turn decodes, but the event must also carry the session post-image it
		// bumped: a turn projected without its session would leave the session's
		// version and timestamps stale.
		{
			"turn with no session post-image", state.EvTurnAppended,
			`{"turn":{"ID":"t1","SessionID":"s1","Seq":1,"Role":"user","Content":"hi"}}`,
		},
		{"skill with no post-image", state.EvSkillUpserted, `{}`},
		{"memory item with no post-image", state.EvMemoryWritten, `{}`},
		{"unknown event type", "state.invented", `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			appendRaw(t, s, state.StateStream, tc.typ, tc.payload)
			if err := s.Rebuild(ctx); err == nil {
				t.Fatalf("rebuild folded an unprojectable %q event without error", tc.typ)
			}
		})
	}
}

// TestRebuildFailsWhenAProjectionTableIsMissing proves the fold reports a projection it
// could not perform. Each case writes a real record through the live path, removes the
// table the fold would project it into, and asserts the rebuild fails: a rebuild that
// returned success here would advertise a reconciled projection that does not exist.
func TestRebuildFailsWhenAProjectionTableIsMissing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		populate func(*Store)
		table    string
	}{
		{"turns", func(s *Store) {
			ses, err := s.Sessions().Create(ctx, state.Session{Title: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Sessions().AppendTurn(ctx, state.Turn{SessionID: ses.ID, Role: "user", Content: "hi"}); err != nil {
				t.Fatal(err)
			}
		}, "turns"},
		{"skills", func(s *Store) {
			if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"}); err != nil {
				t.Fatal(err)
			}
		}, "skills"},
		{"memory items", func(s *Store) {
			if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"}); err != nil {
				t.Fatal(err)
			}
		}, "memory_items"},
		{"memory usage", func(s *Store) {
			it, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Memory().RecordPush(ctx, []string{it.ID}); err != nil {
				t.Fatal(err)
			}
		}, "memory_usage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			tc.populate(s)
			dropTables(t, s, tc.table)
			if err := s.Rebuild(ctx); err == nil {
				t.Fatalf("rebuild succeeded with the %s table missing", tc.table)
			}
		})
	}
}

// TestSnapshotRestoreFailsWhenATableIsMissing covers the other half of a resumed rebuild:
// the restore step replays the snapshot's records through the same row writers the fold
// uses, so a table it cannot write must fail the rebuild rather than let the fold continue
// on top of a projection that was never restored.
func TestSnapshotRestoreFailsWhenATableIsMissing(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"sessions", "turns", "skills", "memory_items"} {
		t.Run(table, func(t *testing.T) {
			s := newStore(t)
			populateSQLiteState(t, s)
			if err := s.SnapshotState(ctx); err != nil {
				t.Fatal(err)
			}
			dropTables(t, s, table)
			if err := s.Rebuild(ctx); err == nil {
				t.Fatalf("rebuild from a snapshot succeeded with the %s table missing", table)
			}
		})
	}
}

// TestSnapshotStateFailsWhenATableIsMissing gates the checkpoint read: a state snapshot is
// the whole projection, so a table it cannot read makes the snapshot incomplete. It must
// fail rather than save a partial snapshot, which a later rebuild would restore as a
// silently truncated projection. The events table is included: without it the snapshot
// cannot be anchored at the stream head, and an unanchored snapshot would make a rebuild
// re-fold or skip the wrong events.
func TestSnapshotStateFailsWhenATableIsMissing(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"sessions", "turns", "skills", "memory_items", "events"} {
		t.Run(table, func(t *testing.T) {
			s := newStore(t)
			populateSQLiteState(t, s)
			dropTables(t, s, table)
			if err := s.SnapshotState(ctx); err == nil {
				t.Fatalf("SnapshotState succeeded with the %s table missing", table)
			}
		})
	}
}

// refusingCodec is a snapshot codec that refuses to seal. It stands for a signer that is
// unavailable or a key that is not held: with a codec configured, an unsealed snapshot must
// never reach storage.
type refusingCodec struct{}

var errSealRefused = errors.New("sqlite: test codec refuses to seal")

func (refusingCodec) Seal(context.Context, spine.Log, spine.Snapshot) (spine.Snapshot, error) {
	return spine.Snapshot{}, errSealRefused
}

func (refusingCodec) Open(_ context.Context, s spine.Snapshot) (spine.Snapshot, error) { return s, nil }

// TestSnapshotFailsWhenTheCodecRefusesToSeal proves the verified-snapshot promise holds on
// the write side too: with a codec configured, a snapshot that cannot be sealed is not
// saved unsealed. Both streams are checked, because both would otherwise store a payload
// no verifier could reject.
func TestSnapshotFailsWhenTheCodecRefusesToSeal(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	s := newStore(t, WithSnapshotCodec(refusingCodec{}))
	populateSQLiteState(t, s)
	rs := s.Resources(reg)
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w", Spec: json.RawMessage(`{"size":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.SnapshotState(ctx); !errors.Is(err, errSealRefused) {
		t.Fatalf("SnapshotState error = %v, want the codec's seal failure", err)
	}
	if err := rs.Snapshot(ctx); !errors.Is(err, errSealRefused) {
		t.Fatalf("resource Snapshot error = %v, want the codec's seal failure", err)
	}
	// Nothing was stored on either stream: a refused seal leaves no snapshot behind.
	for _, stream := range []string{state.StateStream, resource.ResourceStream} {
		if _, found, err := s.Log().LatestSnapshot(ctx, stream, 0); err != nil || found {
			t.Fatalf("stream %q has a stored snapshot after a refused seal (found=%v err=%v)", stream, found, err)
		}
	}
}

// TestRebuildSkipsAnUndecodableSnapshot is the derived-cache guarantee: a stored snapshot
// whose payload is garbage is not an error, it is a cache miss. The rebuild ignores it,
// folds the stream from the start, and lands the same projection - only slower, never
// wrong.
func TestRebuildSkipsAnUndecodableSnapshot(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	s := newStore(t)
	rs := s.Resources(reg)
	populateSQLiteState(t, s)
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w", Spec: json.RawMessage(`{"size":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	live := dumpState(t, s)

	for _, stream := range []string{state.StateStream, resource.ResourceStream} {
		if err := s.Log().SaveSnapshot(ctx, spine.Snapshot{
			Stream: stream, Seq: 99, Payload: []byte("not a snapshot"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A garbage snapshot must not be consulted: the fold restarts from seq 0.
	if _, afterSeq := s.stateSnapshotForRebuild(ctx); afterSeq != 0 {
		t.Fatal("an undecodable state snapshot was accepted instead of falling back to a full fold")
	}
	if restored, afterSeq := rs.(*resourceStore).snapshotForRebuild(ctx); afterSeq != 0 || restored != nil {
		t.Fatal("an undecodable resource snapshot was accepted instead of falling back to a full fold")
	}

	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("state rebuild past an undecodable snapshot: %v", err)
	}
	if err := rs.(*resourceStore).Rebuild(ctx); err != nil {
		t.Fatalf("resource rebuild past an undecodable snapshot: %v", err)
	}
	if got := dumpState(t, s); got != live {
		t.Fatalf("rebuild past an undecodable snapshot changed the projection:\n live=%s\n got=%s", live, got)
	}
	if got, err := rs.Get(ctx, "Widget", resource.Scope{}, "w"); err != nil || got.Name != "w" {
		t.Fatalf("resource after the fallback rebuild = (%+v, %v), want the live record", got, err)
	}
}

// TestAutomaticStateSnapshotCadence covers the state stream's self-checkpointing: after
// every k committed mutations the store writes a snapshot on its own, anchored at the
// stream head, so a rebuild folds at most k events past it. It is counted per committed
// mutation, so the count below is exact rather than approximate.
func TestAutomaticStateSnapshotCadence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithSnapshotEvery(3))

	// Two mutations: below the cadence, nothing is checkpointed yet.
	if _, err := s.Sessions().Create(ctx, state.Session{Title: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Log().LatestSnapshot(ctx, state.StateStream, 0); err != nil || found {
		t.Fatalf("snapshot written after 2 of 3 mutations (found=%v err=%v)", found, err)
	}

	// The third mutation reaches the cadence and checkpoints the stream at its head.
	if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"}); err != nil {
		t.Fatal(err)
	}
	snap, found, err := s.Log().LatestSnapshot(ctx, state.StateStream, 0)
	if err != nil || !found {
		t.Fatalf("the cadence wrote no snapshot after 3 mutations (found=%v err=%v)", found, err)
	}
	if snap.Seq != 3 {
		t.Fatalf("snapshot anchored at seq %d, want the stream head (3)", snap.Seq)
	}
	decoded, err := state.UnmarshalSnapshot(snap.Payload)
	if err != nil {
		t.Fatalf("the automatic snapshot does not decode: %v", err)
	}
	if len(decoded.Sessions) != 1 || len(decoded.Skills) != 1 || len(decoded.Items) != 1 {
		t.Fatalf("automatic snapshot holds %d sessions, %d skills, %d items; want 1 of each",
			len(decoded.Sessions), len(decoded.Skills), len(decoded.Items))
	}
}

// TestRecallScopedWithNoQuery covers the agent-startup read: a recall with a scope and no
// search text is the scoped, no-FTS shape that runs on the prepared statement. It must
// return only the items in that scope, newest first, and honour a limit; an unlimited
// recall on the same shape must return the whole scope.
func TestRecallScopedWithNoQuery(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mine := state.Scope{Project: "alpha"}
	theirs := state.Scope{Project: "beta"}

	for _, c := range []string{"first", "second", "third"} {
		if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: c, Scope: mine}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "other", Scope: theirs}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Memory().Recall(ctx, state.RecallQuery{Scope: mine})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unlimited scoped recall = %d items, want the 3 in scope", len(all))
	}
	for _, it := range all {
		if it.Scope != mine {
			t.Fatalf("scoped recall returned an item from scope %+v", it.Scope)
		}
	}
	capped, err := s.Memory().Recall(ctx, state.RecallQuery{Scope: mine, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("scoped recall with Limit 2 = %d items, want 2", len(capped))
	}
}

// TestSkillSearchAppliesTheLimit covers the FTS branch's capped shape: a text search with a
// limit matches the same rows an uncapped one does, cut to the limit, so a caller asking
// for the top k does not silently get everything.
func TestSkillSearchAppliesTheLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, slug := range []string{"a", "b", "c"} {
		if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: slug, Body: "deploy the service"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "z", Body: "unrelated"}); err != nil {
		t.Fatal(err)
	}

	full, err := s.Skills().Search(ctx, "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 {
		t.Fatalf("uncapped search = %d hits, want the 3 matching skills", len(full))
	}
	capped, err := s.Skills().Search(ctx, "deploy", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("search with limit 2 = %d hits, want 2", len(capped))
	}
	if capped[0].Slug != full[0].Slug || capped[1].Slug != full[1].Slug {
		t.Fatalf("the capped search returned different rows than the uncapped prefix")
	}
}

// TestInsertEventTxResolvesPayload pins the command path's payload rule: a raw payload is
// stored verbatim (it is already the JSON the stamper serialized, so the durable path never
// re-encodes what it just encoded), a decoded payload is marshalled, supplying both is a
// conflict, and a payload that cannot be marshalled fails the write. Getting this wrong
// would either double-encode a post-image or commit an event whose payload is empty.
func TestInsertEventTxResolvesPayload(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Both forms at once is a conflict: there would be two candidate post-images and no
	// rule that says which one the log recorded.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := insertEventTx(ctx, tx, s, spine.AppendInput{
			Stream: "run/x", Type: "t", Actor: spine.ActorAgent,
			Payload: map[string]any{"a": 1}, RawPayload: json.RawMessage(`{"a":2}`),
		})
		return err
	})
	if !errors.Is(err, spine.ErrPayloadConflict) {
		t.Fatalf("both payload forms = %v, want spine.ErrPayloadConflict", err)
	}

	// A payload that cannot be marshalled fails the write rather than committing an
	// event with an empty body.
	err = s.tx(ctx, func(tx *sql.Tx) error {
		_, err := insertEventTx(ctx, tx, s, spine.AppendInput{
			Stream: "run/x", Type: "t", Actor: spine.ActorAgent,
			Payload: map[string]any{"ch": make(chan int)},
		})
		return err
	})
	if err == nil {
		t.Fatal("an unmarshallable payload was accepted, want a failure")
	}

	// A decoded payload is marshalled; an unset Time and SchemaVersion are defaulted.
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		seq, err := insertEventTx(ctx, tx, s, spine.AppendInput{
			Stream: "run/x", Type: "decoded", Actor: spine.ActorAgent,
			Payload: map[string]any{"k": "v"},
		})
		if err != nil {
			return err
		}
		if seq != 1 {
			t.Fatalf("first event on the stream got seq %d, want 1", seq)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A raw payload is stored as given, on the next seq of the same stream.
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		seq, err := insertEventTx(ctx, tx, s, spine.AppendInput{
			Stream: "run/x", Type: "raw", Actor: spine.ActorAgent,
			RawPayload: json.RawMessage(`{"k":"raw"}`),
		})
		if err != nil {
			return err
		}
		if seq != 2 {
			t.Fatalf("second event got seq %d, want 2", seq)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	evs, err := s.Log().Read(ctx, spine.Query{Stream: "run/x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("stream holds %d events, want the 2 that committed", len(evs))
	}
	if evs[0].Payload["k"] != "v" || evs[1].Payload["k"] != "raw" {
		t.Fatalf("payloads read back as %v and %v", evs[0].Payload, evs[1].Payload)
	}
	if evs[0].Time.IsZero() || evs[0].SchemaVersion != spine.DefaultSchemaVersion {
		t.Fatalf("event was not defaulted: time=%v version=%d", evs[0].Time, evs[0].SchemaVersion)
	}
}

// TestAppendRejectsBothPayloadForms is the same conflict rule on the public Append path: an
// input carrying a decoded and a raw payload is refused before anything is written.
func TestAppendRejectsBothPayloadForms(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Log().Append(ctx, spine.AppendInput{
		Stream: "run/x", Type: "t", Actor: spine.ActorAgent,
		Payload: map[string]any{"a": 1}, RawPayload: json.RawMessage(`{"a":2}`),
	}); err == nil {
		t.Fatal("Append accepted both payload forms, want a rejection")
	}
	if evs, err := s.Log().Read(ctx, spine.Query{Stream: "run/x"}); err != nil || len(evs) != 0 {
		t.Fatalf("the rejected append left %d events behind (err=%v)", len(evs), err)
	}
}

// TestReadFailsOnACorruptPayload proves a read never invents a payload: an event row whose
// stored body is not JSON surfaces as an error naming the failure, rather than an event
// with an empty or partial payload that a fold would happily project.
func TestReadFailsOnACorruptPayload(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	appendRaw(t, s, "run/corrupt", "t", `{"ok":true}`)
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET payload = ? WHERE stream = ?`, "}not json{", "run/corrupt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Log().Read(ctx, spine.Query{Stream: "run/corrupt"}); err == nil {
		t.Fatal("read of a corrupt payload succeeded, want a failure")
	}
}

// TestResourceRebuildRejectsUnprojectableEvents is the resource stream's fail-closed fold,
// the mirror of the state stream's: an event whose post-image does not decode, or whose
// type the projection does not know, aborts the rebuild rather than leaving the table
// reconciled to only part of the log.
func TestResourceRebuildRejectsUnprojectableEvents(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	tests := []struct {
		name    string
		typ     string
		payload string
	}{
		{"undecodable post-image", resource.EvPut, `{"resource":42}`},
		{"undecodable merged post-image", resource.EvMerged, `{"resource":"nope"}`},
		{"unknown event type", "resource.invented", `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			rs := s.Resources(reg).(*resourceStore)
			appendRaw(t, s, resource.ResourceStream, tc.typ, tc.payload)
			if err := rs.Rebuild(ctx); err == nil {
				t.Fatalf("rebuild folded an unprojectable %q event without error", tc.typ)
			}
		})
	}
}

// TestCorruptResourceJSONColumnsDecodeToNil pins the row decode's tolerance boundary. The
// labels, annotations, finalizers, and owner-reference columns are JSON text; a column that
// does not parse yields an empty field, so one bad column can never panic a list or take
// the whole record out of the projection. The record's identity and its schema-validated
// spec, which the content hash commits to, are unaffected.
func TestCorruptResourceJSONColumnsDecodeToNil(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	s := newStore(t)
	rs := s.Resources(reg)

	saved, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w",
		Labels: map[string]string{"tier": "pro"}, Spec: json.RawMessage(`{"size":"m"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE resources SET labels = ?, annotations = ?, finalizers = ?, owner_references = ? WHERE id = ?`,
		"{oops", "{oops", "[oops", "[oops", saved.ID); err != nil {
		t.Fatal(err)
	}

	got, err := rs.Get(ctx, "Widget", resource.Scope{}, "w")
	if err != nil {
		t.Fatalf("get over corrupt JSON columns: %v", err)
	}
	if got.ID != saved.ID || got.ContentHash != saved.ContentHash {
		t.Fatalf("identity changed: id=%q hash=%q, want id=%q hash=%q", got.ID, got.ContentHash, saved.ID, saved.ContentHash)
	}
	if got.Labels != nil || got.Annotations != nil || got.Finalizers != nil || got.OwnerReferences != nil {
		t.Fatalf("corrupt columns decoded to non-empty values: %+v", got)
	}
	// A list over the same row still works, and a selector simply matches nothing:
	// the row has no labels to match once its label column is unreadable.
	all, err := rs.ListAll(ctx, "Widget", nil)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll over a corrupt row = %d rows (err=%v), want 1", len(all), err)
	}
	sel := resource.Selector{{Key: "tier", Op: resource.OpEquals, Values: []string{"pro"}}}
	hits, err := rs.ListAll(ctx, "Widget", sel)
	if err != nil || len(hits) != 0 {
		t.Fatalf("selector over a corrupt label column = %d rows (err=%v), want 0", len(hits), err)
	}
}

// TestWriterActorDefaultsToTheAgent pins the provenance column's default: the writer actor
// is never stored empty, so every projected record names who wrote it and a merge can
// compare provenance without a special case for "unset".
func TestWriterActorDefaultsToTheAgent(t *testing.T) {
	if got := writerActorOrDefault(""); got != spine.ActorAgent {
		t.Fatalf("writerActorOrDefault(\"\") = %q, want %q", got, spine.ActorAgent)
	}
	if got := writerActorOrDefault(spine.ActorSystem); got != spine.ActorSystem {
		t.Fatalf("writerActorOrDefault(system) = %q, want it preserved", got)
	}
}

// TestWarmBodiesRoundTripThroughTheReadPath ties the tiers together from the read side: an
// event whose body was relocated to the warm store reads back with the exact payload it was
// appended with, decompressed on the way out, while a body still hot reads inline. The two
// tiers are indistinguishable to a reader.
func TestWarmBodiesRoundTripThroughTheReadPath(t *testing.T) {
	ctx := context.Background()
	const stream = "run/tiers"
	s := newStore(t, WithPayloadBlobThreshold(1))
	body := strings.Repeat("payload ", 1024)

	appendRaw(t, s, stream, "tool.result", `{"body":"`+body+`"}`)
	appendRaw(t, s, stream, "tool.result", `{"body":"second"}`)
	if err := s.SaveCheckpoint(ctx, stream, 1, []byte("cose")); err != nil {
		t.Fatal(err)
	}
	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil || moved != 1 {
		t.Fatalf("archive moved %d bodies (err=%v), want 1 (only the sealed event)", moved, err)
	}

	evs, err := s.Log().Read(ctx, spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("stream read = %d events, want 2", len(evs))
	}
	if evs[0].Payload["body"] != body {
		t.Fatal("the warm-tier body did not rehydrate to the bytes it was appended with")
	}
	if evs[1].Payload["body"] != "second" {
		t.Fatal("the still-hot body did not read back verbatim")
	}
}

// TestMemoryUsageReadsRejectACorruptRow: a usage row whose counter is not a number
// fails the read rather than being skipped or read as zero. SQLite's dynamic typing
// lets such a row exist, and a usage read that quietly returned nothing for it would
// report a heavily pushed item as one nobody has ever seen.
func TestMemoryUsageReadsRejectACorruptRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	it, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Memory().RecordPush(ctx, []string{it.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_usage SET push_count = 'not a number'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Memory().Usage(ctx, nil); err == nil {
		t.Fatal("Usage read a row whose counter is not a number")
	}
	// The write path reads the stored row back before incrementing it, so it has to
	// refuse the same row rather than start the counter over from zero.
	if err := s.Memory().RecordPush(ctx, []string{it.ID}); err == nil {
		t.Fatal("RecordPush overwrote a row it could not read back")
	}
}
