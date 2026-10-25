package spinetest_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/conformtest"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spine/spinetest"
)

// faulty wraps a correct log and lets a case replace one behaviour.
type faulty struct {
	spine.Log
	appendFn func(in spine.Log, a spine.AppendInput) (spine.Event, error)
	readFn   func(in spine.Log, q spine.Query) ([]spine.Event, error)
	saveFn   func(in spine.Log, s spine.Snapshot) error
	latestFn func(in spine.Log, stream string, upTo int64) (spine.Snapshot, bool, error)
}

func (f *faulty) Append(ctx context.Context, a spine.AppendInput) (spine.Event, error) {
	if f.appendFn != nil {
		return f.appendFn(f.Log, a)
	}
	return f.Log.Append(ctx, a)
}

func (f *faulty) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	if f.readFn != nil {
		return f.readFn(f.Log, q)
	}
	return f.Log.Read(ctx, q)
}

func (f *faulty) SaveSnapshot(ctx context.Context, s spine.Snapshot) error {
	if f.saveFn != nil {
		return f.saveFn(f.Log, s)
	}
	return f.Log.SaveSnapshot(ctx, s)
}

func (f *faulty) LatestSnapshot(ctx context.Context, stream string, upTo int64) (spine.Snapshot, bool, error) {
	if f.latestFn != nil {
		return f.latestFn(f.Log, stream, upTo)
	}
	return f.Log.LatestSnapshot(ctx, stream, upTo)
}

func TestSuiteFailsOnBrokenLog(t *testing.T) {
	ctx := context.Background()
	c := func(name, want string, brk func(f *faulty)) conformtest.Case {
		return conformtest.Case{Name: name, Want: want, Suite: func(t *testing.T) {
			spinetest.RunSuite(t, func() spine.Log {
				f := &faulty{Log: spine.NewMemoryLog()}
				brk(f)
				return f
			})
		}}
	}
	// afterAppend edits the event Append returns.
	afterAppend := func(edit func(*spine.Event)) func(f *faulty) {
		return func(f *faulty) {
			f.appendFn = func(in spine.Log, a spine.AppendInput) (spine.Event, error) {
				e, err := in.Append(ctx, a)
				edit(&e)
				return e, err
			}
		}
	}
	// beforeAppend edits the input Append passes on.
	beforeAppend := func(edit func(*spine.AppendInput)) func(f *faulty) {
		return func(f *faulty) {
			f.appendFn = func(in spine.Log, a spine.AppendInput) (spine.Event, error) {
				edit(&a)
				return in.Append(ctx, a)
			}
		}
	}
	// afterRead edits every event Read returns.
	afterRead := func(edit func(*spine.Event)) func(f *faulty) {
		return func(f *faulty) {
			f.readFn = func(in spine.Log, q spine.Query) ([]spine.Event, error) {
				got, err := in.Read(ctx, q)
				for i := range got {
					edit(&got[i])
				}
				return got, err
			}
		}
	}

	conformtest.Prove(t, []conformtest.Case{
		c("sequence repeats", "PerStreamMonotonicSeq", afterAppend(func(e *spine.Event) { e.Seq = (e.Seq + 1) / 2 })),
		c("sequence is global across streams", "StreamsIndependent", func(f *faulty) {
			var n atomic.Int64
			f.appendFn = func(in spine.Log, a spine.AppendInput) (spine.Event, error) {
				e, err := in.Append(ctx, a)
				e.Seq = n.Add(1)
				return e, err
			}
		}),
		c("read ignores AfterSeq", "ReadAfterAndLimit", func(f *faulty) {
			f.readFn = func(in spine.Log, q spine.Query) ([]spine.Event, error) {
				q.AfterSeq = 0
				return in.Read(ctx, q)
			}
		}),
		c("read ignores Limit", "ReadAfterAndLimit", func(f *faulty) {
			f.readFn = func(in spine.Log, q spine.Query) ([]spine.Event, error) {
				q.Limit = 0
				return in.Read(ctx, q)
			}
		}),
		c("zero time is not stamped", "Time", afterAppend(func(e *spine.Event) { e.Time = time.Time{} })),
		c("stored time loses precision", "Time", afterRead(func(e *spine.Event) { e.Time = e.Time.Truncate(time.Second) })),
		c("payload aliases the caller's map", "PayloadImmutableAndPreserved", func(f *faulty) {
			var mu sync.Mutex
			held := map[int64]map[string]any{}
			f.appendFn = func(in spine.Log, a spine.AppendInput) (spine.Event, error) {
				e, err := in.Append(ctx, a)
				mu.Lock()
				held[e.Seq] = a.Payload
				mu.Unlock()
				return e, err
			}
			f.readFn = func(in spine.Log, q spine.Query) ([]spine.Event, error) {
				got, err := in.Read(ctx, q)
				mu.Lock()
				defer mu.Unlock()
				for i := range got {
					got[i].Payload = held[got[i].Seq]
				}
				return got, err
			}
		}),
		c("principal is dropped", "PayloadImmutableAndPreserved", beforeAppend(func(a *spine.AppendInput) { a.Principal = "" })),
		c("causation link is dropped", "PayloadImmutableAndPreserved", beforeAppend(func(a *spine.AppendInput) { a.CausationID = "" })),
		c("both payload forms accepted", "RawPayload", beforeAppend(func(a *spine.AppendInput) {
			if a.Payload != nil {
				a.RawPayload = nil
			}
		})),
		c("raw payload not decoded on return", "RawPayload", afterAppend(func(e *spine.Event) { e.Payload = nil })),
		c("default schema version not applied", "SchemaVersion", afterAppend(func(e *spine.Event) { e.SchemaVersion = 0 })),
		c("explicit schema version lost on read", "SchemaVersion", afterRead(func(e *spine.Event) { e.SchemaVersion = spine.DefaultSchemaVersion })),
		c("unknown stream is an error", "EmptyStream", func(f *faulty) {
			f.readFn = func(in spine.Log, q spine.Query) ([]spine.Event, error) {
				got, err := in.Read(ctx, q)
				if err == nil && len(got) == 0 {
					err = errors.New("no such stream")
				}
				return got, err
			}
		}),
		c("appends are silently dropped", "Concurrency", func(f *faulty) {
			var n atomic.Int64
			f.appendFn = func(in spine.Log, a spine.AppendInput) (spine.Event, error) {
				if n.Add(1)%7 == 0 {
					return spine.Event{}, nil
				}
				return in.Append(ctx, a)
			}
		}),
		c("latest snapshot is the last saved", "Snapshots", func(f *faulty) {
			var mu sync.Mutex
			last := map[string]spine.Snapshot{}
			f.saveFn = func(in spine.Log, s spine.Snapshot) error {
				mu.Lock()
				last[s.Stream] = s
				mu.Unlock()
				return in.SaveSnapshot(ctx, s)
			}
			f.latestFn = func(in spine.Log, stream string, upTo int64) (spine.Snapshot, bool, error) {
				mu.Lock()
				defer mu.Unlock()
				s, ok := last[stream]
				return s, ok, nil
			}
		}),
		c("latest snapshot ignores the upper bound", "Snapshots", func(f *faulty) {
			f.latestFn = func(in spine.Log, stream string, _ int64) (spine.Snapshot, bool, error) {
				return in.LatestSnapshot(ctx, stream, 0)
			}
		}),
		c("snapshots leak across streams", "Snapshots", func(f *faulty) {
			f.latestFn = func(in spine.Log, _ string, upTo int64) (spine.Snapshot, bool, error) {
				return in.LatestSnapshot(ctx, "s", upTo)
			}
		}),
		c("snapshot payload aliases the caller's slice", "Snapshots", func(f *faulty) {
			var mu sync.Mutex
			held := map[int64][]byte{}
			f.saveFn = func(in spine.Log, s spine.Snapshot) error {
				mu.Lock()
				held[s.Seq] = s.Payload
				mu.Unlock()
				return in.SaveSnapshot(ctx, s)
			}
			f.latestFn = func(in spine.Log, stream string, upTo int64) (spine.Snapshot, bool, error) {
				s, ok, err := in.LatestSnapshot(ctx, stream, upTo)
				mu.Lock()
				defer mu.Unlock()
				if ok {
					s.Payload = held[s.Seq]
				}
				return s, ok, err
			}
		}),
	})
}
