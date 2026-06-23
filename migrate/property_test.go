package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/fstest"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/migrate"
)

// buildMigrations returns n migrations 0001..000n, each creating table t<i>.
func buildMigrations(n int) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 1; i <= n; i++ {
		fsys[fmt.Sprintf("%04d_t.sql", i)] = &fstest.MapFile{
			Data: []byte(fmt.Sprintf("CREATE TABLE t%d(id INTEGER);", i)),
		}
	}
	return fsys
}

// TestProp_PartialThenCompleteIsConsistent: for any size and any split point,
// applying a prefix and then the full set is equivalent to applying the full set
// directly — the recorded count matches, every table exists, and re-running is a
// no-op. This exercises the resume path and idempotency across generated sizes.
func TestProp_PartialThenCompleteIsConsistent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(rt, "n")
		k := rapid.IntRange(0, n).Draw(rt, "k")
		ctx := context.Background()

		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			rt.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer func() { _ = db.Close() }()

		if err := migrate.Run(ctx, db, buildMigrations(k)); err != nil {
			rt.Fatalf("partial(%d): %v", k, err)
		}
		if got := appliedCount(rt, db); got != k {
			rt.Fatalf("after partial: applied %d, want %d", got, k)
		}
		if err := migrate.Run(ctx, db, buildMigrations(n)); err != nil {
			rt.Fatalf("full(%d): %v", n, err)
		}
		if got := appliedCount(rt, db); got != n {
			rt.Fatalf("after full: applied %d, want %d", got, n)
		}
		// Idempotent.
		if err := migrate.Run(ctx, db, buildMigrations(n)); err != nil {
			rt.Fatalf("re-run: %v", err)
		}
		if got := appliedCount(rt, db); got != n {
			rt.Fatalf("re-run changed count to %d", got)
		}
		for i := 1; i <= n; i++ {
			if _, err := db.Exec(fmt.Sprintf("INSERT INTO t%d(id) VALUES (1)", i)); err != nil {
				rt.Fatalf("table t%d missing: %v", i, err)
			}
		}
	})
}

func appliedCount(rt *rapid.T, db *sql.DB) int {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		rt.Fatal(err)
	}
	return n
}

// TestFaultRecoveryResumesAfterFailure injects a broken migration mid-sequence —
// chaos for a migration runner — and asserts it fails closed at the last good
// version, then resumes correctly once the migration is fixed. This proves the
// per-migration-commit forward-progress and recovery.
func TestFaultRecoveryResumesAfterFailure(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	broken := fstest.MapFS{
		"0001_a.sql": {Data: []byte(`CREATE TABLE t1(id INTEGER);`)},
		"0002_b.sql": {Data: []byte(`CREATE TABLE t2(id INTEGER);`)},
		"0003_c.sql": {Data: []byte(`CREATE TABLE t3(id INTEGER); BROKEN SQL HERE;`)},
		"0004_d.sql": {Data: []byte(`CREATE TABLE t4(id INTEGER);`)},
	}
	if err := migrate.Run(ctx, db, broken); err == nil {
		t.Fatal("expected failure at 0003")
	}
	if n := countApplied(t, db); n != 2 {
		t.Fatalf("applied %d, want 2 (stopped at the break)", n)
	}

	fixed := fstest.MapFS{
		"0001_a.sql": broken["0001_a.sql"],
		"0002_b.sql": broken["0002_b.sql"],
		"0003_c.sql": {Data: []byte(`CREATE TABLE t3(id INTEGER);`)},
		"0004_d.sql": broken["0004_d.sql"],
	}
	if err := migrate.Run(ctx, db, fixed); err != nil {
		t.Fatalf("resume after fix: %v", err)
	}
	if n := countApplied(t, db); n != 4 {
		t.Fatalf("applied %d, want 4 after resume", n)
	}
}
