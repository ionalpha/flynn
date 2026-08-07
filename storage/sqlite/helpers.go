package sqlite

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/internal/sqlitex"
)

func formatTime(t time.Time) string { return sqlitex.FormatTime(t) }

func parseTime(s string) time.Time { return sqlitex.ParseTime(s) }

// hlcTime reconstructs an hlc.Time from its stored columns. The counter column
// only ever holds a uint16 written by this package; the mask makes that explicit
// (and satisfies the integer-overflow checker).
func hlcTime(wall, counter int64) hlc.Time {
	return hlc.Time{Wall: wall, Counter: uint16(counter & 0xffff)}
}

// ftsPhrase wraps a user query as a single FTS5 phrase so arbitrary input is
// matched literally and can never be misread as FTS5 query syntax. Internal
// double quotes are doubled per the FTS5 string-literal rules.
func ftsPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
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
