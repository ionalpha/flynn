package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// migrationsBefore0029 is the embedded migration set as it stood before the
// timestamp rewrite, so a test can build a database in the old encoding and then
// migrate it forward the way a real installation does.
func migrationsBefore0029(t *testing.T) fs.FS {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, n := range names {
		if base := strings.TrimPrefix(n, "migrations/"); base >= "0029" {
			continue
		}
		body, err := fs.ReadFile(migrations, n)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = &fstest.MapFile{Data: body}
	}
	if len(out) == 0 {
		t.Fatal("no migrations before 0029")
	}
	return out
}

// legacyTime is the pre-0029 on-disk encoding: RFC3339Nano, trailing zeros of
// the fractional second stripped.
func legacyTime(ts time.Time) string { return ts.UTC().Format(time.RFC3339Nano) }

// timestampColumns is every column migration 0029 rewrites, paired with the
// statement that puts one row carrying that timestamp into its table.
var timestampColumns = []struct {
	table  string
	column string
}{
	{"sessions", "created_at"},
	{"sessions", "updated_at"},
	{"turns", "created_at"},
	{"skills", "created_at"},
	{"skills", "updated_at"},
	{"memory_items", "created_at"},
	{"events", "time"},
	{"resources", "created_at"},
	{"resources", "updated_at"},
	{"resources", "valid_from"},
	{"resources", "valid_to"},
	{"resources", "deletion_timestamp"},
}

// seedLegacyRows writes one row per table with every timestamp column set to ts
// in the old encoding. id carries through so a row can be found again.
func seedLegacyRows(ctx context.Context, t *testing.T, db *sql.DB, id string, ts time.Time) {
	t.Helper()
	old := legacyTime(ts)
	env := `0, 'local', 0, 0, 'local'` // sync_version, origin, hlc wall/counter, last writer

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO sessions (id, created_at, updated_at, sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id)
	      VALUES (?, ?, ?, `+env+`)`, id, old, old)
	exec(`INSERT INTO turns (id, session_id, seq, created_at, sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id)
	      VALUES (?, ?, 1, ?, `+env+`)`, id, id, old)
	exec(`INSERT INTO skills (id, slug, version, created_at, updated_at, sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id)
	      VALUES (?, ?, 1, ?, ?, `+env+`)`, id, id, old, old)
	exec(`INSERT INTO memory_items (id, created_at, sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id)
	      VALUES (?, ?, `+env+`)`, id, old)
	exec(`INSERT INTO events (stream, seq, time) VALUES ('s', (SELECT COUNT(*) + 1 FROM events), ?)`, old)
	exec(`INSERT INTO resources (id, api_version, kind, name, version, created_at, updated_at, valid_from, valid_to, deletion_timestamp, sync_version, origin_instance_id, updated_hlc_wall, updated_hlc_counter, last_writer_id)
	      VALUES (?, 'v1', 'Thing', ?, 1, ?, ?, ?, ?, ?, `+env+`)`, id, id, old, old, old, old, old)
}

// legacyTimestamps are the instants the migration has to fix, chosen for the
// fractional-digit counts RFC3339Nano renders differently: none at all (the
// case that sorted wrongly), one, three, and a full nine.
var legacyTimestamps = []time.Time{
	time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 100000000, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 123000000, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 987654321, time.UTC),
}

// TestMigration0029RewritesLegacyTimestamps opens a database in the pre-0029
// encoding, migrates it, and asserts every timestamp column now holds the padded
// form, that the instant is unchanged, and that re-running the migration changes
// nothing.
func TestMigration0029RewritesLegacyTimestamps(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy.db")

	old, err := sqlitex.Open(ctx, dsn, migrationsBefore0029(t))
	if err != nil {
		t.Fatalf("open at 0028: %v", err)
	}
	for i, ts := range legacyTimestamps {
		seedLegacyRows(ctx, t, old, string(rune('a'+i)), ts)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sqlitex.Open(ctx, dsn, migrations)
	if err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	defer func() { _ = db.Close() }()

	want := make(map[string]bool, len(legacyTimestamps))
	for _, ts := range legacyTimestamps {
		want[sqlitex.FormatTime(ts)] = true
	}
	for _, c := range timestampColumns {
		rows, err := db.QueryContext(ctx, `SELECT `+c.column+` FROM `+c.table+` WHERE `+c.column+` IS NOT NULL`)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.column, err)
		}
		n := 0
		for rows.Next() {
			var got string
			if err := rows.Scan(&got); err != nil {
				t.Fatalf("%s.%s scan: %v", c.table, c.column, err)
			}
			if len(got) != 30 {
				t.Errorf("%s.%s = %q, %d chars, want the 30-char padded form", c.table, c.column, got, len(got))
			}
			if !want[got] {
				t.Errorf("%s.%s = %q, which is not one of the seeded instants: the rewrite changed the value, not just its width", c.table, c.column, got)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s.%s rows: %v", c.table, c.column, err)
		}
		if n != len(legacyTimestamps) {
			t.Errorf("%s.%s: read %d rows, want %d", c.table, c.column, n, len(legacyTimestamps))
		}
	}

	// The migrated column sorts by instant. Before 0029 the zero-nanosecond row
	// came back last here, because '.' sorts before 'Z'.
	var first string
	if err := db.QueryRowContext(ctx, `SELECT created_at FROM memory_items ORDER BY created_at LIMIT 1`).Scan(&first); err != nil {
		t.Fatalf("ordered read: %v", err)
	}
	if got, want := sqlitex.ParseTime(first), legacyTimestamps[0]; !got.Equal(want) {
		t.Fatalf("earliest row by SQL order = %v, want %v", got, want)
	}

	// Idempotence: re-running the whole set (a fresh open replays nothing, so run
	// the file's statements again directly) leaves every value as it is.
	before := columnDigest(ctx, t, db)
	body, err := fs.ReadFile(migrations, "migrations/0029_fixed_width_timestamps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("re-run 0029: %v", err)
	}
	if after := columnDigest(ctx, t, db); after != before {
		t.Fatalf("re-running 0029 changed the data:\nbefore %s\nafter  %s", before, after)
	}
}

// columnDigest concatenates every rewritten timestamp in a stable order, so two
// readings can be compared for "nothing moved".
func columnDigest(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range timestampColumns {
		rows, err := db.QueryContext(ctx, `SELECT COALESCE(`+c.column+`, '') FROM `+c.table+` ORDER BY 1`)
		if err != nil {
			t.Fatalf("digest %s.%s: %v", c.table, c.column, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("digest scan: %v", err)
			}
			sb.WriteString(c.table + "." + c.column + "=" + v + ";")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("digest rows: %v", err)
		}
		_ = rows.Close()
	}
	return sb.String()
}
