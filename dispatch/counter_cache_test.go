package dispatch_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/observe"
)

var errRejected = errors.New("rejected")

// countingMeter records how many times each named instrument is resolved, so a test
// can prove the dispatcher resolves its lifecycle counters once at construction rather
// than per governed call.
type countingMeter struct {
	mu      sync.Mutex
	lookups map[string]int
}

func (m *countingMeter) Counter(name string) observe.Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookups == nil {
		m.lookups = map[string]int{}
	}
	m.lookups[name]++
	return countingCounter{}
}

func (m *countingMeter) Histogram(string) observe.Histogram { return nopHistogram{} }

func (m *countingMeter) count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookups[name]
}

type countingCounter struct{}

func (countingCounter) Add(context.Context, int64, ...observe.Field) {}

type nopHistogram struct{}

func (nopHistogram) Record(context.Context, float64, ...observe.Field) {}

// TestCountersResolvedOncePerDispatcher pins the waist optimization: dispatch.tokens and
// dispatch.actions are resolved by name exactly once, at construction, then reused. Govern
// is the chokepoint every tool, model, and spawn flows through, so a per-call name lookup
// would be hot; this asserts the lookup count stays flat as governed calls pile up.
func TestCountersResolvedOncePerDispatcher(t *testing.T) {
	m := &countingMeter{}
	ob := &observe.Observability{Log: observe.NopLogger{}, Tracer: observe.NopTracer{}, Meter: m}
	d := dispatch.New(dispatch.WithObservability(ob))

	// Both counters are resolved exactly once during New, before any Govern.
	if got := m.count("dispatch.tokens"); got != 1 {
		t.Fatalf("dispatch.tokens resolved %d times at construction, want 1", got)
	}
	if got := m.count("dispatch.actions"); got != 1 {
		t.Fatalf("dispatch.actions resolved %d times at construction, want 1", got)
	}

	// Governing many actions (both the success and rejection paths) must not resolve
	// either counter again.
	for range 25 {
		called := false
		_ = d.Govern(context.Background(), dispatch.Action{Name: "fetch"}, okWork(&called, 7))
	}
	dr := dispatch.New(dispatch.WithObservability(ob), dispatch.WithAdmitter(denyAdmitter{err: errRejected}))
	// A second dispatcher resolves its own pair once; account for it below.
	for range 25 {
		called := false
		_ = dr.Govern(context.Background(), dispatch.Action{Name: "fetch"}, okWork(&called, 7))
	}

	// Two dispatchers, one resolution each, zero per governed call: 2 lookups per name.
	if got := m.count("dispatch.tokens"); got != 2 {
		t.Errorf("dispatch.tokens resolved %d times across 2 dispatchers + 50 Govern calls, want 2", got)
	}
	if got := m.count("dispatch.actions"); got != 2 {
		t.Errorf("dispatch.actions resolved %d times across 2 dispatchers + 50 Govern calls, want 2", got)
	}
}
