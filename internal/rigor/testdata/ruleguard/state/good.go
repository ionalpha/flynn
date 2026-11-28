package state

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ionalpha/flynn/observe"
)

func logAndHandle(ctx context.Context, log observe.Logger, err error) error {
	log.Error(ctx, "failed, continuing without it")
	return nil
}

func returnWithoutLogging(err error) error {
	return fmt.Errorf("load: %w", err)
}

func infoThenReturn(ctx context.Context, log observe.Logger, err error) error {
	log.Info(ctx, "retrying")
	return err
}

func paddedTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func secondTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTimestamp(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func stable(s []string) {
	sort.SliceStable(s, func(i, j int) bool { return len(s[i]) < len(s[j]) })
	slices.SortStableFunc(s, strings.Compare)
}
