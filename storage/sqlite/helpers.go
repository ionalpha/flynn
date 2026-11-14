package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/internal/sqlitex"
	"github.com/ionalpha/flynn/recallplan"
)

func formatTime(t time.Time) string { return sqlitex.FormatTime(t) }

func parseTime(s string) time.Time { return sqlitex.ParseTime(s) }

// hlcTime reconstructs an hlc.Time from its stored columns. The counter column
// only ever holds a uint16 written by this package; the mask makes that explicit
// (and satisfies the integer-overflow checker).
func hlcTime(wall, counter int64) hlc.Time {
	return hlc.Time{Wall: wall, Counter: uint16(counter & 0xffff)}
}

// ftsMatch renders one planned attempt as an FTS5 MATCH expression. Each term is
// quoted as a string literal, with internal double quotes doubled, so a word that
// is an FTS5 operator (AND, NOT, NEAR) or carries syntax (a colon, a hyphen, a
// parenthesis) is matched as text and can never be read as query syntax. A bare
// word and its quoted form are the same match, so quoting every term costs nothing.
// Adjacent literals are an implicit AND in FTS5; an attempt that accepts any term
// joins them with OR.
func ftsMatch(a recallplan.Attempt) string {
	sep := " "
	if a.Any {
		sep = " OR "
	}
	quoted := make([]string, len(a.Terms))
	for i, t := range a.Terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, sep)
}

// selectByMemoryID reads cols from table for the named items, or every row when
// memoryIDs is empty, and scans each row with scan. orderBy is appended verbatim.
//
// The ids go in as placeholders built to match their count, never interpolated, so the
// caller can pass whatever a digest hands it. cols, table and orderBy are package
// constants and literals, which is what lets them be concatenated.
func selectByMemoryID[T any](
	ctx context.Context,
	db interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	},
	cols, table, orderBy string,
	memoryIDs []string,
	scan func(interface{ Scan(...any) error }) (T, error),
) ([]T, error) {
	var q strings.Builder
	q.WriteString(`SELECT ` + cols + ` FROM ` + table)
	args := make([]any, 0, len(memoryIDs))
	for i, id := range memoryIDs {
		if i == 0 {
			q.WriteString(` WHERE memory_id IN (`)
		} else {
			q.WriteString(`, `)
		}
		q.WriteString(`?`)
		args = append(args, id)
	}
	if len(memoryIDs) > 0 {
		q.WriteString(`)`)
	}
	q.WriteString(` ORDER BY ` + orderBy)
	rows, err := db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]T, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}
