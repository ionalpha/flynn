package sqlite

// What every write does when the table it needs is gone. The log and the projections
// move together: if the event cannot be appended, the projection must not be written
// either, so a dropped events table fails every mutating call rather than quietly
// leaving a row the log does not explain.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
	"github.com/ionalpha/flynn/state"
)

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
		{"memory_promotions", []string{"memory_promotions"}, func(s *Store) error {
			it, err := s.Memory().Write(ctx, state.MemoryItem{Kind: "f", Content: "c"})
			if err != nil {
				return err
			}
			// The decision's lookup of what is already on file runs inside the writing
			// transaction, so a lookup that errors must surface rather than read as
			// "nobody has decided yet" and overwrite an answer it could not see.
			_, err = s.Memory().Promote(ctx, state.PromotionDecision{MemoryID: it.ID, Promoted: true, By: "user:operator"})
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
