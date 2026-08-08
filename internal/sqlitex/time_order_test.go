package sqlitex_test

import (
	"context"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// timeOrderMigrations is a one-column table to sort formatted times in.
var timeOrderMigrations = fstest.MapFS{
	"migrations/0001_init.sql": &fstest.MapFile{
		Data: []byte(`CREATE TABLE stamps (id INTEGER PRIMARY KEY, ts TEXT NOT NULL);`),
	},
}

// orderingCases mixes instants that land exactly on a second with instants a
// nanosecond either side of one, which is where the unpadded encoding inverted
// the order: the zero-nanosecond string ends in 'Z' (0x5A) where the others
// carry a '.' (0x2E) in the same position.
var orderingCases = []time.Time{
	time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 0, 999999999, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	time.Date(2026, 1, 1, 0, 0, 1, 500000000, time.UTC),
	time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
}

// TestSQLOrderMatchesTimeOrder is the regression test for the ordering bug: rows
// written through FormatTime come back from `ORDER BY ts` in instant order. It
// fails on the pre-0029 encoding, where the two instants one nanosecond apart
// around a second boundary come back swapped.
func TestSQLOrderMatchesTimeOrder(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, ":memory:", timeOrderMigrations)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Inserted in an order unrelated to time, so the sort is doing the work.
	for i, ts := range []int{3, 0, 5, 1, 4, 2} {
		if _, err := db.ExecContext(ctx, "INSERT INTO stamps (id, ts) VALUES (?, ?)", i, sqlitex.FormatTime(orderingCases[ts])); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT ts FROM stamps ORDER BY ts")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []time.Time
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, sqlitex.ParseTime(s))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(orderingCases) {
		t.Fatalf("got %d rows, want %d", len(got), len(orderingCases))
	}
	for i, want := range orderingCases { // orderingCases is written in ascending order
		if !got[i].Equal(want) {
			t.Fatalf("row %d = %v, want %v (SQL order disagrees with time order)", i, got[i], want)
		}
	}
}

// TestUnpaddedEncodingInvertsOrder pins the defect the padded layout fixes, so
// the reason for the layout cannot quietly stop being true: RFC3339Nano, the old
// on-disk format, sorts a later instant before an earlier one.
func TestUnpaddedEncodingInvertsOrder(t *testing.T) {
	onTheSecond := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oneNanoLater := onTheSecond.Add(time.Nanosecond)

	if a, b := onTheSecond.Format(time.RFC3339Nano), oneNanoLater.Format(time.RFC3339Nano); a <= b {
		t.Fatalf("RFC3339Nano %q < %q: the ordering defect this layout works around is gone, and the padding may no longer be needed", a, b)
	}
	if a, b := sqlitex.FormatTime(onTheSecond), sqlitex.FormatTime(oneNanoLater); a >= b {
		t.Fatalf("FormatTime %q >= %q, but the earlier instant must sort first", a, b)
	}
}

// TestFormatTimeIsFixedWidth asserts the property the migration's guard relies
// on: every formatted value is exactly 30 characters, so a stored value of that
// length is already migrated and re-running 0029 is a no-op.
func TestFormatTimeIsFixedWidth(t *testing.T) {
	for _, ts := range orderingCases {
		if got := sqlitex.FormatTime(ts); len(got) != 30 {
			t.Fatalf("FormatTime(%v) = %q, %d chars, want 30", ts, got, len(got))
		}
	}
}

// Property: for any set of instants, SQL's lexicographic order over the stored
// strings is the instant order. This is the guarantee the recall queries and
// every other ORDER BY over a timestamp column depend on.
func TestProp_SQLOrderIsTimeOrder(t *testing.T) {
	ctx := context.Background()
	rapid.Check(t, func(rt *rapid.T) {
		// Second-resolution draws with a separately drawn nanosecond, so "exactly
		// on the second" is generated often rather than once in a billion.
		times := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) time.Time {
			sec := rapid.Int64Range(1735689600, 1798761600).Draw(t, "sec") // 2025 to 2027
			nsec := rapid.SampledFrom([]int64{0, 1, 999999999, 500000000, 123000000}).Draw(t, "nsec")
			return time.Unix(sec, nsec).UTC()
		}), 2, 12).Draw(rt, "times")

		db, err := sqlitex.Open(ctx, ":memory:", timeOrderMigrations)
		if err != nil {
			rt.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()

		for i, ts := range times {
			if _, err := db.ExecContext(ctx, "INSERT INTO stamps (id, ts) VALUES (?, ?)", i, sqlitex.FormatTime(ts)); err != nil {
				rt.Fatalf("insert: %v", err)
			}
		}

		want := append([]time.Time(nil), times...)
		sort.Slice(want, func(i, j int) bool { return want[i].Before(want[j]) })

		rows, err := db.QueryContext(ctx, "SELECT ts FROM stamps ORDER BY ts, id")
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		defer func() { _ = rows.Close() }()

		i := 0
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rt.Fatalf("scan: %v", err)
			}
			if got := sqlitex.ParseTime(s); !got.Equal(want[i]) {
				rt.Fatalf("row %d = %v, want %v", i, got, want[i])
			}
			i++
		}
		if err := rows.Err(); err != nil {
			rt.Fatalf("rows: %v", err)
		}
		if i != len(want) {
			rt.Fatalf("read %d rows, want %d", i, len(want))
		}
	})
}
