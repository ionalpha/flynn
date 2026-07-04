//go:build !race

// Allocation ceilings for the disabled-log path. The standalone Default logger is a
// true no-op, and a host-injected slog logger short-circuits a record whose level is
// disabled before building attributes, so a log call threaded through a hot path that
// is turned off must cost nothing. Excluded under -race (instrumentation skews
// allocation counts); dev/bench and the CI bench job run it.

package observe

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestAllocCeilingNopLoggerDisabledPath(t *testing.T) {
	ctx := context.Background()
	log := Default().Log
	fields := []Field{String("run", "r1"), Int("n", 3)}
	allocs := testing.AllocsPerRun(100, func() {
		log.Info(ctx, "processed step", fields...)
	})
	if allocs != 0 {
		t.Errorf("standalone Default logger allocates %.0f/op, want 0: the disabled-log path must be free", allocs)
	}
}

func TestAllocCeilingSlogDisabledLevel(t *testing.T) {
	ctx := context.Background()
	// Handler enabled at Info: a Debug record must be dropped by the Enabled guard
	// before any attribute slice is built.
	log := NewSlogLogger(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	fields := []Field{String("run", "r1"), Int("n", 3)}
	allocs := testing.AllocsPerRun(100, func() {
		log.Debug(ctx, "trace detail", fields...)
	})
	if allocs != 0 {
		t.Errorf("slog Debug at a disabled level allocates %.0f/op, want 0: the Enabled guard must short-circuit before attrs", allocs)
	}
}
