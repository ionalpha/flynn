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
		"memory.Usage": func() error { _, err := s.Memory().Usage(ctx, nil); return err },
		"memory.Promote": func() error {
			_, err := s.Memory().Promote(ctx, state.PromotionDecision{MemoryID: "id", Promoted: true, By: "user:operator"})
			return err
		},
		"memory.Promotions":    func() error { _, err := s.Memory().Promotions(ctx, nil); return err },
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
