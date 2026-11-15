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

// hlcTime masks the counter, always a uint16 written here, for the
// integer-overflow checker.
func hlcTime(wall, counter int64) hlc.Time {
	return hlc.Time{Wall: wall, Counter: uint16(counter & 0xffff)}
}

// ftsMatch quotes every term, doubling inner double quotes, so an FTS5
// operator or syntax character (AND, NEAR, colon, hyphen) is matched as text.
// Adjacent literals are an implicit AND; an attempt accepting any term joins
// them with OR.
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

// selectByMemoryID reads every row when memoryIDs is empty. Ids are bound
// placeholders, never interpolated. cols, table and orderBy are appended
// verbatim, so they must be package constants.
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
