package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/sqlitex"
)

// Property: FormatTime and ParseTime are exact inverses for any instant in any
// zone - the round trip lands on the same nanosecond, normalised to UTC. The
// canonical on-disk time format loses nothing.
func TestProp_TimeRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sec := rapid.Int64Range(-62135596800, 253402300799).Draw(rt, "sec") // year 1 to 9999
		nsec := rapid.Int64Range(0, 999999999).Draw(rt, "nsec")
		offset := rapid.IntRange(-12, 14).Draw(rt, "tzHours")
		in := time.Unix(sec, nsec).In(time.FixedZone("gen", offset*3600))

		got := sqlitex.ParseTime(sqlitex.FormatTime(in))
		if !got.Equal(in) {
			rt.Fatalf("round trip of %v = %v", in, got)
		}
		if got.Location() != time.UTC {
			rt.Fatalf("parsed location = %v, want UTC", got.Location())
		}
	})
}

// Property: ParseTime never panics on arbitrary input, and anything it does not
// reject round-trips: a non-zero result re-formats to a string that parses to
// the same instant.
func TestProp_ParseTimeTotal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		got := sqlitex.ParseTime(s)
		if got.IsZero() {
			return // rejected: the documented total-function fallback
		}
		again := sqlitex.ParseTime(sqlitex.FormatTime(got))
		if !again.Equal(got) {
			rt.Fatalf("accepted %q as %v, which re-round-trips to %v", s, got, again)
		}
	})
}

// propMigrations is the minimal schema the transaction property writes into.
var propMigrations = fstest.MapFS{
	"migrations/0001_init.sql": &fstest.MapFile{
		Data: []byte(`CREATE TABLE rows (id INTEGER PRIMARY KEY AUTOINCREMENT, batch INTEGER NOT NULL);`),
	},
}

// Property: Tx is atomic over any sequence of batches - a batch whose function
// fails leaves no rows behind, whatever it wrote first, so the table ends up
// holding exactly the successful batches' rows.
func TestProp_TxAtomicOverBatchSequence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		db, err := sqlitex.Open(ctx, ":memory:", propMigrations)
		if err != nil {
			rt.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()

		type batch struct {
			rows int
			fail bool
		}
		batches := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) batch {
			return batch{
				rows: rapid.IntRange(0, 5).Draw(t, "rows"),
				fail: rapid.Bool().Draw(t, "fail"),
			}
		}), 0, 8).Draw(rt, "batches")

		want := 0
		for i, b := range batches {
			err := sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
				for range b.rows {
					if _, err := tx.ExecContext(ctx, "INSERT INTO rows (batch) VALUES (?)", i); err != nil {
						return fmt.Errorf("insert: %w", err)
					}
				}
				if b.fail {
					return errors.New("batch aborted")
				}
				return nil
			})
			if b.fail {
				if err == nil {
					rt.Fatalf("batch %d: Tx swallowed the failure", i)
				}
			} else {
				if err != nil {
					rt.Fatalf("batch %d: Tx failed: %v", i, err)
				}
				want += b.rows
			}
		}

		var got int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rows").Scan(&got); err != nil {
			rt.Fatalf("count: %v", err)
		}
		if got != want {
			rt.Fatalf("table holds %d rows, want %d (only committed batches)", got, want)
		}

		// Rows from failed batches must not exist at all, not just be uncounted.
		var badRows int
		for i, b := range batches {
			if !b.fail {
				continue
			}
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rows WHERE batch = ?", i).Scan(&badRows); err != nil {
				rt.Fatalf("count batch %d: %v", i, err)
			}
			if badRows != 0 {
				rt.Fatalf("failed batch %d left %d rows behind", i, badRows)
			}
		}
	})
}
