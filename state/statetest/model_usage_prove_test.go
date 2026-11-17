package statetest_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/state/statetest"
	"github.com/ionalpha/flynn/usage"
)

// faultyUsage wraps a correct store and lets a case replace one operation, so
// each defect is one function and the rest of the store stays correct.
type faultyUsage struct {
	state.UsageStore
	onAppend func(ctx context.Context, in state.UsageStore, rs ...usage.Record) error
	onByRun  func(ctx context.Context, in state.UsageStore, run string) ([]usage.Record, error)
	onTree   func(ctx context.Context, in state.UsageStore, root string) ([]usage.Record, error)
	onWindow func(ctx context.Context, in state.UsageStore, from, to time.Time, principal string) ([]usage.Record, error)
}

func (f *faultyUsage) Append(ctx context.Context, rs ...usage.Record) error {
	if f.onAppend != nil {
		return f.onAppend(ctx, f.UsageStore, rs...)
	}
	return f.UsageStore.Append(ctx, rs...)
}

func (f *faultyUsage) ByRun(ctx context.Context, run string) ([]usage.Record, error) {
	if f.onByRun != nil {
		return f.onByRun(ctx, f.UsageStore, run)
	}
	return f.UsageStore.ByRun(ctx, run)
}

func (f *faultyUsage) ByTree(ctx context.Context, root string) ([]usage.Record, error) {
	if f.onTree != nil {
		return f.onTree(ctx, f.UsageStore, root)
	}
	return f.UsageStore.ByTree(ctx, root)
}

func (f *faultyUsage) Window(ctx context.Context, from, to time.Time, principal string) ([]usage.Record, error) {
	if f.onWindow != nil {
		return f.onWindow(ctx, f.UsageStore, from, to, principal)
	}
	return f.UsageStore.Window(ctx, from, to, principal)
}

// listUsage keeps every record it is given, in a list: no key, no validation.
type listUsage struct {
	mu   sync.Mutex
	recs []usage.Record
}

func (l *listUsage) Append(_ context.Context, rs ...usage.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, rs...)
	return nil
}

func (l *listUsage) all() []usage.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.recs)
	state.SortUsageRecords(out)
	return out
}

func (l *listUsage) ByRun(_ context.Context, run string) ([]usage.Record, error) {
	return slices.DeleteFunc(l.all(), func(r usage.Record) bool { return r.Run != run }), nil
}

func (l *listUsage) ByTree(ctx context.Context, root string) ([]usage.Record, error) {
	return l.ByRun(ctx, root)
}

func (l *listUsage) Window(context.Context, time.Time, time.Time, string) ([]usage.Record, error) {
	return l.all(), nil
}

func TestModelUsageSuiteFailsOnBrokenStore(t *testing.T) {
	c := func(name, want string, brk func(f *faultyUsage)) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) {
			statetest.RunUsageStoreSuite(t, func() state.UsageStore {
				f := &faultyUsage{UsageStore: statetest.NewMemoryUsageStore()}
				brk(f)
				return f
			})
		}}
	}
	// swallow turns one class of error from the inner store into success.
	swallow := func(target error) func(f *faultyUsage) {
		return func(f *faultyUsage) {
			f.onAppend = func(ctx context.Context, in state.UsageStore, rs ...usage.Record) error {
				if err := in.Append(ctx, rs...); err != nil && !errors.Is(err, target) {
					return err
				}
				return nil
			}
		}
	}
	conformtest.Prove(t, []conformtest.Case{
		{Name: "keeps every record it is sent", Want: "IdempotentAppend", Suite: func(t *testing.T) {
			statetest.RunUsageStoreSuite(t, func() state.UsageStore { return &listUsage{} })
		}},
		c("a differing record under a stored key is dropped without error", "ConflictRefused", swallow(state.ErrConflict)),
		c("invalid records are dropped without error", "InvalidRefused", swallow(state.ErrInvalid)),
		c("a batch stops at the first bad record and keeps the earlier ones", "BatchIsAtomic", func(f *faultyUsage) {
			f.onAppend = func(ctx context.Context, in state.UsageStore, rs ...usage.Record) error {
				for _, r := range rs {
					if err := in.Append(ctx, r); err != nil {
						return err
					}
				}
				return nil
			}
		}),
		c("a run's total includes the runs it spawned", "ByRun", func(f *faultyUsage) {
			f.onByRun = func(ctx context.Context, in state.UsageStore, run string) ([]usage.Record, error) {
				return in.ByTree(ctx, run)
			}
		}),
		c("a tree is only its root", "ByTree", func(f *faultyUsage) {
			f.onTree = func(ctx context.Context, in state.UsageStore, root string) ([]usage.Record, error) {
				return in.ByRun(ctx, root)
			}
		}),
		c("an empty root selects every run with no parent", "ByTree", func(f *faultyUsage) {
			f.onTree = func(ctx context.Context, in state.UsageStore, root string) ([]usage.Record, error) {
				if root != "" {
					return in.ByTree(ctx, root)
				}
				all, err := in.Window(ctx, time.Time{}, time.Time{}, "")
				return slices.DeleteFunc(all, func(r usage.Record) bool { return r.ParentRun != "" }), err
			}
		}),
		c("a window includes its end", "Window", func(f *faultyUsage) {
			f.onWindow = func(ctx context.Context, in state.UsageStore, from, to time.Time, p string) ([]usage.Record, error) {
				if !to.IsZero() {
					to = to.Add(time.Nanosecond)
				}
				return in.Window(ctx, from, to, p)
			}
		}),
		c("a window ignores the principal", "Window", func(f *faultyUsage) {
			f.onWindow = func(ctx context.Context, in state.UsageStore, from, to time.Time, _ string) ([]usage.Record, error) {
				return in.Window(ctx, from, to, "")
			}
		}),
		c("a window drops unpriced records", "SumParity", func(f *faultyUsage) {
			f.onWindow = func(ctx context.Context, in state.UsageStore, from, to time.Time, p string) ([]usage.Record, error) {
				got, err := in.Window(ctx, from, to, p)
				return slices.DeleteFunc(got, func(r usage.Record) bool { return !r.Cost.Known }), err
			}
		}),
		c("a window returns newest first", "Order", func(f *faultyUsage) {
			f.onWindow = func(ctx context.Context, in state.UsageStore, from, to time.Time, p string) ([]usage.Record, error) {
				got, err := in.Window(ctx, from, to, p)
				slices.Reverse(got)
				return got, err
			}
		}),
		c("times come back in a local zone", "TimeZones", func(f *faultyUsage) {
			f.onByRun = func(ctx context.Context, in state.UsageStore, run string) ([]usage.Record, error) {
				got, err := in.ByRun(ctx, run)
				for i := range got {
					got[i].At = got[i].At.In(time.FixedZone("UTC+7", 7*60*60))
				}
				return got, err
			}
		}),
		c("times lose their sub-second part", "RoundTrip", func(f *faultyUsage) {
			f.onByRun = func(ctx context.Context, in state.UsageStore, run string) ([]usage.Record, error) {
				got, err := in.ByRun(ctx, run)
				for i := range got {
					got[i].At = got[i].At.Truncate(time.Second)
				}
				return got, err
			}
		}),
	})
}
