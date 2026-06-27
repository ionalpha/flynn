package serve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// flakyStatsSource models a runtime whose load endpoint misbehaves: it refuses the
// connection, hangs until the caller's deadline, hands back impossible garbage, or answers
// cleanly, cycling through these on each call. The manager must absorb all of it, always
// returning a normalized, stamped snapshot and never an error or a panic, so a hung or
// hostile runtime cannot break a control-plane read.
type flakyStatsSource struct {
	mu sync.Mutex
	n  int
}

func (f *flakyStatsSource) Stats(ctx context.Context, _ string) (RuntimeStats, error) {
	f.mu.Lock()
	i := f.n
	f.n++
	f.mu.Unlock()

	switch i % 4 {
	case 0:
		return RuntimeStats{}, errors.New("connection refused")
	case 1:
		// A hung endpoint: block until the caller's deadline, then report the failure. The
		// manager passes the caller context straight through, so this is bounded by the
		// caller's own timeout.
		<-ctx.Done()
		return RuntimeStats{}, ctx.Err()
	case 2:
		return RuntimeStats{Known: true, KVCacheUsage: 7, RequestsRunning: -9, RequestsWaiting: -1, DecodeTokensPerSec: 1e18}, nil
	default:
		return RuntimeStats{Known: true, KVCacheUsage: 0.5, RequestsRunning: 1, RequestsWaiting: 1}, nil
	}
}

// TestStatsAbsorbsFaultySource hammers Stats from many goroutines against the flaky source
// and asserts every reading is safe, normalized, stamped, and bounded in time, with no
// error ever surfaced.
func TestStatsAbsorbsFaultySource(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	if err := reg.Put(Record{ModelID: "m", Runtime: "llama.cpp", BaseURL: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, OSKiller, reg,
		withClock(clock.NewManual(time.Unix(1000, 0))),
		WithStatsSource("llama.cpp", &flakyStatsSource{}))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				s, err := m.Stats(ctx, "m")
				cancel()
				if err != nil {
					t.Errorf("Stats surfaced an error from a faulty source: %v", err)
					return
				}
				if s.KVCacheUsage < 0 || s.KVCacheUsage > 1 {
					t.Errorf("KVCacheUsage out of range: %v", s.KVCacheUsage)
					return
				}
				if s.RequestsRunning < 0 || s.RequestsWaiting < 0 {
					t.Errorf("negative counts: %+v", s)
					return
				}
				if s.DecodeTokensPerSec < 0 {
					t.Errorf("negative throughput: %v", s.DecodeTokensPerSec)
					return
				}
				if s.CollectedAt.Unix() != 1000 {
					t.Errorf("snapshot not stamped from the manager clock: %d", s.CollectedAt.Unix())
					return
				}
			}
		}()
	}
	wg.Wait()
}
