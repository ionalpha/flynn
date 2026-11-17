package statetest

import (
	"context"
	"sync"
	"time"

	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/usage"
)

// NewMemoryModelUsageStore returns a [state.ModelUsageStore] held in memory, for tests
// that need a usage store without a database and as the reference
// [RunModelUsageSuite] holds other stores to. It shares the append rules of
// [state.PlanModelUsageAppend] with the durable stores; the queries are its own, so
// the suite is what proves they agree.
func NewMemoryModelUsageStore() state.ModelUsageStore {
	return &memoryModelUsageStore{recs: map[modelUsageIdent]usage.Record{}}
}

type modelUsageIdent struct{ run, call string }

type memoryModelUsageStore struct {
	mu   sync.Mutex
	recs map[modelUsageIdent]usage.Record
}

func (m *memoryModelUsageStore) Append(_ context.Context, records ...usage.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fresh, err := state.PlanModelUsageAppend(records, func(run, call string) (usage.Record, bool, error) {
		r, ok := m.recs[modelUsageIdent{run, call}]
		return r, ok, nil
	})
	if err != nil {
		return err
	}
	for _, r := range fresh {
		m.recs[modelUsageIdent{r.Run, r.Call}] = r
	}
	return nil
}

// pick returns the records keep accepts, in store order. The caller holds mu.
func (m *memoryModelUsageStore) pick(keep func(usage.Record) bool) []usage.Record {
	out := []usage.Record{}
	for _, r := range m.recs {
		if keep(r) {
			out = append(out, r)
		}
	}
	state.SortModelUsageRecords(out)
	return out
}

func (m *memoryModelUsageStore) ByRun(_ context.Context, run string) ([]usage.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pick(func(r usage.Record) bool { return r.Run == run }), nil
}

func (m *memoryModelUsageStore) ByTree(_ context.Context, root string) ([]usage.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if root == "" {
		return []usage.Record{}, nil
	}
	// Grow the set of runs until no record names a run in it as its parent. A
	// run joins at most once, so a ParentRun cycle ends the walk.
	in := map[string]bool{root: true}
	for grew := true; grew; {
		grew = false
		for _, r := range m.recs {
			if in[r.ParentRun] && !in[r.Run] {
				in[r.Run] = true
				grew = true
			}
		}
	}
	return m.pick(func(r usage.Record) bool { return in[r.Run] }), nil
}

func (m *memoryModelUsageStore) Window(_ context.Context, from, to time.Time, principal string) ([]usage.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pick(func(r usage.Record) bool {
		switch {
		case !from.IsZero() && r.At.Before(from):
			return false
		case !to.IsZero() && !r.At.Before(to):
			return false
		}
		return principal == "" || r.Principal == principal
	}), nil
}
