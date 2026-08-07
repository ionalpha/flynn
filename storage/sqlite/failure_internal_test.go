package sqlite

// Failure paths of the durable backend. The invariant these gate is a single one: the
// store never answers a broken database with a plausible-looking zero value. Every read
// and every write must report the failure, so a caller can never mistake "the table is
// gone" or "the handle is closed" for "there is nothing here". The faults are injected at
// the storage layer itself (a closed handle, a dropped table, an unopenable file), which
// is the only place the SQL error branches can be reached from.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// testAt is the fixed instant every test clock is pinned to, so no test reads the wall
// clock and a re-run stamps identical times.
var testAt = time.Unix(1_700_000_000, 0).UTC()

// newStore opens an in-memory store on a manual clock and closes it when the test ends.
func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	opts = append([]Option{WithClock(clock.NewManual(testAt))}, opts...)
	s, err := Open(context.Background(), ":memory:", opts...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// dropTables removes tables from the open database, the fault injection every
// missing-table test uses. Foreign keys are disabled first so a parent table can be
// dropped while a child still references it.
func dropTables(t *testing.T, s *Store, names ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	for _, n := range names {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE `+n); err != nil {
			t.Fatalf("drop %s: %v", n, err)
		}
	}
}

// hash32 builds a merkle-node-sized hash, the only shape PutNode accepts.
func hash32(b byte) []byte { return bytes.Repeat([]byte{b}, merkleHashSize) }

// remoteWidget produces a fully stamped record written by another instance, the shape
// Merge admits. It is minted through a real write on a scratch store rather than
// hand-assembled, so it carries the identity, envelope, and hashes a replicated record
// actually arrives with.
func remoteWidget(t *testing.T, reg *resource.Registry, name, size string) resource.Resource {
	t.Helper()
	src := newStore(t, WithInstanceID("other"))
	r, err := src.Resources(reg).Put(context.Background(), resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: name,
		Spec: json.RawMessage(`{"size":"` + size + `"}`),
	})
	if err != nil {
		t.Fatalf("mint remote widget: %v", err)
	}
	return r
}

// TestClosedStoreOperationsAllReportErrors is the broadest failure gate: once the
// database handle is closed, every read and every write on every domain (sessions,
// skills, memory, the event log, checkpoints, blobs, jobs, resources, snapshots) must
// return an error. A path that swallowed the closed handle would hand a caller an empty
// list or a zero record and look exactly like an empty store.
func TestClosedStoreOperationsAllReportErrors(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)

	// Open, populate, and close. The records exist, so any successful-looking empty
	// answer below is unambiguously wrong.
	s := newStore(t)
	ses, err := s.Sessions().Create(ctx, state.Session{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "blue"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Jobs().Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	rs := s.Resources(reg)
	widget := resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w",
		Spec: json.RawMessage(`{"size":"m"}`),
	}
	merged := remoteWidget(t, reg, "w", "l")

	ops := map[string]func() error{
		"sessions.Create":     func() error { _, err := s.Sessions().Create(ctx, state.Session{}); return err },
		"sessions.Get":        func() error { _, err := s.Sessions().Get(ctx, ses.ID); return err },
		"sessions.List":       func() error { _, err := s.Sessions().List(ctx); return err },
		"sessions.AppendTurn": func() error { _, err := s.Sessions().AppendTurn(ctx, state.Turn{SessionID: ses.ID}); return err },
		"sessions.Turns":      func() error { _, err := s.Sessions().Turns(ctx, ses.ID); return err },
		"sessions.Delete":     func() error { return s.Sessions().Delete(ctx, ses.ID) },

		"skills.Upsert": func() error { _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "x"}); return err },
		"skills.Get":    func() error { _, err := s.Skills().Get(ctx, "deploy"); return err },
		"skills.List":   func() error { _, err := s.Skills().List(ctx, state.Scope{}); return err },
		"skills.Search": func() error { _, err := s.Skills().Search(ctx, "ship", 0); return err },
		"skills.Delete": func() error { return s.Skills().Delete(ctx, "deploy") },

		"memory.Write":  func() error { _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "f"}); return err },
		"memory.Recall": func() error { _, err := s.Memory().Recall(ctx, state.RecallQuery{Query: "blue"}); return err },
		"memory.RecallScoped": func() error {
			_, err := s.Memory().Recall(ctx, state.RecallQuery{Scope: state.Scope{Project: "p"}})
			return err
		},
		"memory.Delete":     func() error { return s.Memory().Delete(ctx, "id") },
		"memory.RecordPush": func() error { return s.Memory().RecordPush(ctx, []string{"id"}) },
		"memory.RecordUse": func() error {
			return s.Memory().RecordUse(ctx, "id", state.UsageOrganic)
		},
		"memory.Usage":         func() error { _, err := s.Memory().Usage(ctx, nil); return err },
		"store.Rebuild":        func() error { return s.Rebuild(ctx) },
		"store.SnapshotState":  func() error { return s.SnapshotState(ctx) },
		"log.Append":           func() error { _, err := s.Log().Append(ctx, spine.AppendInput{Stream: "x", Type: "t"}); return err },
		"log.Read":             func() error { _, err := s.Log().Read(ctx, spine.Query{Stream: state.StateStream}); return err },
		"log.SaveSnapshot":     func() error { return s.Log().SaveSnapshot(ctx, spine.Snapshot{Stream: "x", Seq: 1}) },
		"log.LatestSnapshot":   func() error { _, _, err := s.Log().LatestSnapshot(ctx, "x", 0); return err },
		"store.SaveCheckpoint": func() error { return s.SaveCheckpoint(ctx, "x", 1, []byte("c")) },
		"store.LatestCheckpoint": func() error {
			_, _, _, err := s.LatestCheckpoint(ctx, "x")
			return err
		},
		"store.CheckpointAt": func() error {
			_, _, err := s.CheckpointAt(ctx, "x", 1)
			return err
		},
		"store.PayloadBlobStats": func() error {
			_, _, _, err := s.PayloadBlobStats(ctx)
			return err
		},
		"store.WarmBlobStats": func() error {
			_, _, _, err := s.WarmBlobStats(ctx)
			return err
		},
		"store.ArchiveSealedBlobs": func() error { _, err := s.ArchiveSealedBlobs(ctx); return err },

		"jobs.Enqueue":  func() error { _, err := s.Jobs().Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); return err },
		"jobs.Claim":    func() error { _, err := s.Jobs().Claim(ctx, jobs.ClaimParams{Limit: 1}); return err },
		"jobs.Get":      func() error { _, err := s.Jobs().Get(ctx, "id"); return err },
		"jobs.Complete": func() error { return s.Jobs().Complete(ctx, "id") },
		"jobs.Fail":     func() error { return s.Jobs().Fail(ctx, "id", "boom", 0) },
		"jobs.Recover":  func() error { _, err := s.Jobs().Recover(ctx); return err },

		"resources.Put": func() error { _, err := rs.Put(ctx, widget); return err },
		"resources.Get": func() error { _, err := rs.Get(ctx, "Widget", resource.Scope{}, "w"); return err },
		"resources.GetByID": func() error {
			_, err := rs.(*resourceStore).GetByID(ctx, "id")
			return err
		},
		"resources.List":    func() error { _, err := rs.List(ctx, "Widget", resource.Scope{}, nil); return err },
		"resources.ListAll": func() error { _, err := rs.ListAll(ctx, "Widget", nil); return err },
		"resources.ListKeys": func() error {
			_, err := rs.(resource.KeyLister).ListKeys(ctx, "Widget")
			return err
		},
		"resources.GetAnyScope": func() error {
			_, _, err := rs.(resource.AnyScopeGetter).GetAnyScope(ctx, "Widget", "w")
			return err
		},
		"resources.Delete":   func() error { return rs.Delete(ctx, "Widget", resource.Scope{}, "w") },
		"resources.Merge":    func() error { _, err := rs.Merge(ctx, merged); return err },
		"resources.Snapshot": func() error { return rs.Snapshot(ctx) },
		"resources.Rebuild":  func() error { return rs.(*resourceStore).Rebuild(ctx) },
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Fatalf("%s on a closed store returned no error", name)
			}
		})
	}
}

// TestOpenRejectsAnUnusableDSN proves Open fails loudly when the hot database cannot be
// created at all: a directory path is not a database, and the store must refuse rather
// than hand back a handle that fails on first use.
func TestOpenRejectsAnUnusableDSN(t *testing.T) {
	if _, err := Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Open on a directory path succeeded, want a failure")
	}
}

// TestOpenFailsWhenTheWarmStoreCannotOpen gates the warm tier's wiring: the warm archive
// lives beside the hot file, and if it cannot be opened the store must not come up with a
// nil warm tier. A store that opened anyway would read archived history as a missing body
// later, so the failure has to surface at Open.
func TestOpenFailsWhenTheWarmStoreCannotOpen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "hot.db")
	// A directory sitting exactly where the warm file belongs: the hot database opens
	// and migrates, then the warm schema write fails.
	if err := os.Mkdir(dsn+warmDSNSuffix, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dsn)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded with an unopenable warm store, want a failure")
	}
	if !strings.Contains(err.Error(), "warm") {
		t.Fatalf("error %q does not name the warm store", err)
	}
}

// TestMissingEventsTableFailsEveryWrite is the command path's core guarantee stated as a
// failure: a state or resource mutation is append-and-project in one transaction, so if
// the event cannot be appended the projection must not be written either. With the log
// gone, every mutating call fails instead of quietly writing a row the log does not
// explain.
func TestMissingEventsTableFailsEveryWrite(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	s := newStore(t)
	rs := s.Resources(reg)
	dropTables(t, s, "events")

	widget := resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w",
		Spec: json.RawMessage(`{"size":"m"}`),
	}
	remote := remoteWidget(t, reg, "w", "l")

	if _, err := s.Sessions().Create(ctx, state.Session{Title: "t"}); err == nil {
		t.Error("session create with no events table succeeded")
	}
	if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "k"}); err == nil {
		t.Error("skill upsert with no events table succeeded")
	}
	if _, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "f"}); err == nil {
		t.Error("memory write with no events table succeeded")
	}
	if _, err := rs.Put(ctx, widget); err == nil {
		t.Error("resource put with no events table succeeded")
	}
	// Merge takes its own path to the log (appendMergeEvent), so it is checked apart
	// from Put: a replicated record must not land in the table without its event either.
	if _, err := rs.Merge(ctx, remote); err == nil {
		t.Error("resource merge with no events table succeeded")
	}

	// Nothing was projected: the failed append rolled the whole transaction back.
	if list, err := s.Sessions().List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("sessions after failed writes = %d (err=%v), want none projected", len(list), err)
	}
}

// TestMissingProjectionTablesFailWrites gates the other half of the transaction: an event
// that cannot be projected must fail the write, not commit a log entry the tables never
// received. Each subtest removes exactly one projection table (including the two FTS
// indexes, which are written by the same projection step) and asserts the matching
// mutation reports the failure.
func TestMissingProjectionTablesFailWrites(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		tables []string
		op     func(*Store) error
	}{
		{"skills", []string{"skills"}, func(s *Store) error {
			_, err := s.Skills().Upsert(ctx, state.Skill{Slug: "k", Body: "b"})
			return err
		}},
		{"skills_fts", []string{"skills_fts"}, func(s *Store) error {
			_, err := s.Skills().Upsert(ctx, state.Skill{Slug: "k", Body: "b"})
			return err
		}},
		{"memory_fts", []string{"memory_fts"}, func(s *Store) error {
			_, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "f", Content: "c"})
			return err
		}},
		{"turns", []string{"turns"}, func(s *Store) error {
			ses, err := s.Sessions().Create(ctx, state.Session{Title: "t"})
			if err != nil {
				return err
			}
			_, err = s.Sessions().AppendTurn(ctx, state.Turn{SessionID: ses.ID, Role: "user", Content: "hi"})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			dropTables(t, s, tc.tables...)
			if err := tc.op(s); err == nil {
				t.Fatalf("mutation succeeded with the %v table(s) missing", tc.tables)
			}
		})
	}
}

// TestSkillDeleteFailsWhenTheTableIsMissing pins the delete command path's lookup: it
// resolves the live skill by id or slug inside the transaction, and a lookup that errors
// (rather than finding nothing) must surface, not be read as "already gone".
func TestSkillDeleteFailsWhenTheTableIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Skills().Upsert(ctx, state.Skill{Slug: "deploy"}); err != nil {
		t.Fatal(err)
	}
	dropTables(t, s, "skills")
	if err := s.Skills().Delete(ctx, "deploy"); err == nil {
		t.Fatal("skill delete succeeded with the skills table missing")
	}
}

// TestResourceCASLookupFailuresSurface covers the resource command path's transactional
// lookups: Delete resolves the record by key and Merge by id, both inside the write
// transaction. A lookup that errors must fail the call; read as "no such record" it would
// turn a broken table into a silent not-found (Delete) or into an unconditional overwrite
// of the local record (Merge).
func TestResourceCASLookupFailuresSurface(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	s := newStore(t)
	rs := s.Resources(reg)
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "w",
		Spec: json.RawMessage(`{"size":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	remote := remoteWidget(t, reg, "w", "l")
	dropTables(t, s, "resources")

	if err := rs.Delete(ctx, "Widget", resource.Scope{}, "w"); err == nil {
		t.Error("resource delete succeeded with the resources table missing")
	}
	if _, err := rs.Merge(ctx, remote); err == nil {
		t.Error("resource merge succeeded with the resources table missing")
	}
}

// TestJobsFailWhenTheTableIsMissing holds the work queue to the same rule as the rest of
// the store: a claim over a broken table must report the failure, never return an empty
// batch that a worker would read as "no work to do".
func TestJobsFailWhenTheTableIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	q := s.Jobs()
	if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	dropTables(t, s, "jobs")

	if _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"}); err == nil {
		t.Error("enqueue succeeded with the jobs table missing")
	}
	if got, err := q.Claim(ctx, jobs.ClaimParams{Limit: 1, LeaseFor: int64(time.Minute)}); err == nil {
		t.Errorf("claim returned %d jobs and no error with the jobs table missing", len(got))
	}
	if err := q.Complete(ctx, "any"); err == nil {
		t.Error("complete succeeded with the jobs table missing")
	}
	if err := q.Fail(ctx, "any", "boom", 0); err == nil {
		t.Error("fail succeeded with the jobs table missing")
	}
	if _, err := q.Recover(ctx); err == nil {
		t.Error("recover succeeded with the jobs table missing")
	}
}

// TestExternalizedAppendFailsWithNoBlobStore proves a large payload is never silently
// downgraded to an inline write: with externalization on and the content-addressed store
// gone, the append fails rather than committing an event whose body was dropped.
func TestExternalizedAppendFailsWithNoBlobStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithPayloadBlobThreshold(1))
	dropTables(t, s, "blobs")

	_, err := s.Log().Append(ctx, spine.AppendInput{
		Stream: "run/blob", Type: "tool.result", Actor: spine.ActorAgent, Time: testAt,
		Payload: map[string]any{"body": strings.Repeat("x", 8192)},
	})
	if err == nil {
		t.Fatal("externalized append succeeded with no blobs table, want a failure")
	}
}

// TestArchiveFailsWhenTheWarmStoreIsUnwritable is the copy-then-delete guarantee stated as
// a failure: a body is only deleted from hot once its warm copy is durable, so if the warm
// write fails the sweep must abort with the body still hot, never delete it into a gap.
func TestArchiveFailsWhenTheWarmStoreIsUnwritable(t *testing.T) {
	ctx := context.Background()
	const stream = "run/unwritable"
	s := newStore(t, WithPayloadBlobThreshold(1))
	if _, err := s.Log().Append(ctx, spine.AppendInput{
		Stream: stream, Type: "tool.result", Actor: spine.ActorAgent, Time: testAt,
		Payload: map[string]any{"body": strings.Repeat("x", 8192)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, stream, 1, []byte("cose")); err != nil {
		t.Fatal(err)
	}
	// Break only the warm half: the hot store stays fully usable, so a body deleted
	// from hot here would be a body lost.
	if err := s.warm.db.Close(); err != nil {
		t.Fatal(err)
	}

	if moved, err := s.ArchiveSealedBlobs(ctx); err == nil {
		t.Fatalf("archive moved %d bodies with an unwritable warm store, want a failure", moved)
	}
	// The body is still hot, and the stream still reads.
	if distinct, _, _, err := s.PayloadBlobStats(ctx); err != nil || distinct != 1 {
		t.Fatalf("hot bodies after the failed sweep = %d (err=%v), want 1 (nothing deleted)", distinct, err)
	}
	if evs, err := s.Log().Read(ctx, spine.Query{Stream: stream}); err != nil || len(evs) != 1 {
		t.Fatalf("stream read after the failed sweep = %d events (err=%v), want 1", len(evs), err)
	}
	// No retention event was recorded: a sweep that moved nothing records nothing.
	evs, err := s.Log().Read(ctx, spine.Query{Stream: "retention"})
	if err != nil || len(evs) != 0 {
		t.Fatalf("retention events after the failed sweep = %d (err=%v), want 0", len(evs), err)
	}
}

// TestNoWarmTierIsAQuietNoOp covers the degenerate configuration the warm accessors
// document: with no warm store wired, its accounting is zero and archival moves nothing,
// both without an error, so a caller with no warm tier is not forced to special-case it.
func TestNoWarmTierIsAQuietNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithPayloadBlobThreshold(1))
	if err := s.warm.close(); err != nil {
		t.Fatal(err)
	}
	s.warm = nil

	count, raw, packed, err := s.WarmBlobStats(ctx)
	if err != nil || count != 0 || raw != 0 || packed != 0 {
		t.Fatalf("WarmBlobStats with no warm tier = (%d,%d,%d,%v), want all zero and no error", count, raw, packed, err)
	}
	moved, err := s.ArchiveSealedBlobs(ctx)
	if err != nil || moved != 0 {
		t.Fatalf("ArchiveSealedBlobs with no warm tier = (%d,%v), want (0,nil)", moved, err)
	}
}

// TestWarmGetAndHasReportAbsence pins the warm store's miss semantics: an id it does not
// hold is reported absent, not as an error, so a reader can tell "not archived here" from
// "the archive is broken" and name the id that is genuinely gone.
func TestWarmGetAndHasReportAbsence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	present, err := s.warm.has(ctx, "deadbeef")
	if err != nil || present {
		t.Fatalf("has(unknown) = (%v,%v), want (false,nil)", present, err)
	}
	body, ok, err := s.warm.get(ctx, "deadbeef")
	if err != nil || ok || body != nil {
		t.Fatalf("get(unknown) = (%v,%v,%v), want (nil,false,nil)", body, ok, err)
	}
}

// TestMerkleNodeAbsenceIsNotAnError gates proof assembly: a node the tiled store has not
// recorded reports absent rather than erroring, both for a tile that was never written and
// for a slot past the filled prefix of a resident tile. A tile is a fixed-width blob, so
// "the blob is shorter than this offset" must mean absent, not a slice out of range.
func TestMerkleNodeAbsenceIsNotAnError(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/merkle")

	if _, ok, err := ms.Node(0, 0); err != nil || ok {
		t.Fatalf("Node on an empty store = (ok=%v, err=%v), want absent and no error", ok, err)
	}
	if err := ms.PutNode(0, 0, hash32(0xab)); err != nil {
		t.Fatal(err)
	}
	// Slot 0 of the resident tile is served from memory, verbatim.
	got, ok, err := ms.Node(0, 0)
	if err != nil || !ok || !bytes.Equal(got, hash32(0xab)) {
		t.Fatalf("Node(0,0) = (%x, ok=%v, err=%v), want the stored hash", got, ok, err)
	}
	// Slot 5 lives in the same resident tile but past its filled prefix: absent.
	if _, ok, err := ms.Node(0, 5); err != nil || ok {
		t.Fatalf("Node(0,5) past the filled prefix = (ok=%v, err=%v), want absent and no error", ok, err)
	}
}

// TestMerkleRejectsAWrongWidthHash guards the tile layout: tiles address nodes by fixed
// offset, so a hash of any other width would corrupt every neighbouring slot. It is
// refused at the write, not truncated or padded.
func TestMerkleRejectsAWrongWidthHash(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/merkle")
	if err := ms.PutNode(0, 0, []byte{1, 2, 3}); err == nil {
		t.Fatal("PutNode accepted a 3-byte hash, want a rejection")
	}
}

// TestMerkleFailuresSurfaceOnABrokenStore proves the tiled node store never silently drops
// proof material: with the database closed underneath it, a read of an evicted tile, a
// write that must resume a persisted tile, the write that fills (and so persists) a tile,
// and Flush all report the failure. A swallowed error here would leave a log whose
// checkpoint commits to nodes that were never written.
func TestMerkleFailuresSurfaceOnABrokenStore(t *testing.T) {
	s := newStore(t)
	ms := s.MerkleNodes("run/broken")

	// Fill the tile up to (but not including) its last slot, so it stays resident and
	// the closing write below is the one that persists it.
	for i := range uint64(merkleTileWidth - 1) {
		if err := ms.PutNode(0, i, hash32(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ms.Node(1, 0); err == nil {
		t.Error("Node of a non-resident tile succeeded on a closed store")
	}
	if err := ms.PutNode(1, 0, hash32(0x01)); err == nil {
		t.Error("PutNode that must resume a persisted tile succeeded on a closed store")
	}
	if err := ms.PutNode(0, merkleTileWidth-1, hash32(0xff)); err == nil {
		t.Error("PutNode that fills (and persists) a tile succeeded on a closed store")
	}
	if err := ms.Flush(); err == nil {
		t.Error("Flush succeeded on a closed store")
	}
}

// TestCheckpointLookupsReportAbsence pins the checkpoint reads' miss semantics: a stream
// with no signed head, and a size no head was stored at, both report ok=false with no
// error. A reopening log distinguishes "never checkpointed" from "the read failed", so it
// can start a fresh tree instead of refusing to open.
func TestCheckpointLookupsReportAbsence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, _, ok, err := s.LatestCheckpoint(ctx, "run/none"); err != nil || ok {
		t.Fatalf("LatestCheckpoint of an unchecked stream = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if err := s.SaveCheckpoint(ctx, "run/some", 4, []byte("cose-4")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.CheckpointAt(ctx, "run/some", 9); err != nil || ok {
		t.Fatalf("CheckpointAt a size with no head = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	cose, ok, err := s.CheckpointAt(ctx, "run/some", 4)
	if err != nil || !ok || string(cose) != "cose-4" {
		t.Fatalf("CheckpointAt(4) = (%q, ok=%v, err=%v), want the saved head", cose, ok, err)
	}
}

// constReader is a deterministic entropy source: an id generator built on it and a manual
// clock produces the same ids on every run, which is what makes replay reproducible.
type constReader struct{ b byte }

func (r constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// TestInstanceIDAndInjectedIDGenerator covers the two identity options together: the store
// reports the instance id it stamps onto records (so a running agent can address its own
// Instance resource), and an injected generator is the one the records get their ids from.
// Two stores wired with the same clock and the same entropy therefore mint the same id for
// the same write, which is the basis of deterministic replay.
func TestInstanceIDAndInjectedIDGenerator(t *testing.T) {
	ctx := context.Background()
	newSeeded := func() *Store {
		clk := clock.NewManual(testAt)
		gen := ids.NewGenerator(ids.WithClock(clk), ids.WithEntropy(constReader{b: 0x5a}))
		s, err := Open(ctx, ":memory:", WithClock(clk), WithInstanceID("node-9"), WithIDGenerator(gen))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	a, b := newSeeded(), newSeeded()
	if got := a.InstanceID(); got != "node-9" {
		t.Fatalf("InstanceID() = %q, want node-9", got)
	}
	ska, err := a.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	skb, err := b.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Body: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if ska.ID == "" || ska.ID != skb.ID {
		t.Fatalf("seeded stores minted %q and %q, want one deterministic id", ska.ID, skb.ID)
	}
	if ska.OriginInstanceID != "node-9" {
		t.Fatalf("origin instance = %q, want node-9", ska.OriginInstanceID)
	}
}
