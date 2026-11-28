// Package state holds the examples the project's lint rules must report. Its
// path ends in a name the rules treat as deterministic.
package state

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ionalpha/flynn/observe"
)

func logThenReturn(ctx context.Context, log observe.Logger, err error) error {
	log.Error(ctx, "failed") // want "logged and then returned"
	return err
}

func warnThenReturnPair(ctx context.Context, log observe.Logger, err error) (int, error) {
	if err != nil {
		log.Warn(ctx, "failed", observe.Field{}) // want "logged and then returned"
		return 0, fmt.Errorf("load: %w", err)
	}
	return 1, nil
}

func trimmedTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano) // want "trims trailing zeros"
}

func appendedTimestamp(b []byte, t time.Time) []byte {
	return t.AppendFormat(b, time.RFC3339Nano) // want "trims trailing zeros"
}

func unstable(s []string) {
	sort.Slice(s, func(i, j int) bool { return len(s[i]) < len(s[j]) }) // want "equal elements"
}
