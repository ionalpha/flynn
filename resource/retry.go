package resource

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// maxConflictRetries bounds the optimistic-concurrency retry loops in Update and
// UpdateByID. Each retry re-reads, so a retry is consumed only when another
// writer landed in between; the expected contention on any one resource is a
// handful of writers, and the bound guards a pathological writer that never
// stops, not an expected contention level.
const maxConflictRetries = 1000

// ErrSkipUpdate is returned by an Update mutate function to end the retry loop
// without writing: the state the mutation observed makes the write unnecessary
// (a park already cleared, a limit already exceeded). Update returns the
// resource as read and a nil error.
var ErrSkipUpdate = errors.New("resource: skip update")

// Update applies mutate to the resource addressed by (kind, scope, name) under
// optimistic concurrency: read, mutate, Put, and on ErrConflict re-read and
// reapply against the fresh version. It is the one conflict-retry policy for
// read-modify-write updates, so every caller converges under contention the same
// way instead of hand-rolling its own bound and yield. mutate sees a fresh copy
// each attempt and must be safe to run more than once. A read error, a mutate
// error other than ErrSkipUpdate, and a non-conflict write error end the loop
// and are returned as-is (a caller maps ErrNotFound to its own semantics). An
// exhausted retry budget returns an error matching errors.Is ErrConflict.
func Update(ctx context.Context, store Store, kind string, scope Scope, name string, mutate func(*Resource) error) (Resource, error) {
	return retryOnConflict(ctx, store, kind+"/"+name, mutate, func(ctx context.Context) (Resource, error) {
		return store.Get(ctx, kind, scope, name)
	})
}

// UpdateByID is Update addressed by the resource's stable id.
func UpdateByID(ctx context.Context, store Store, id string, mutate func(*Resource) error) (Resource, error) {
	return retryOnConflict(ctx, store, id, mutate, func(ctx context.Context) (Resource, error) {
		return store.GetByID(ctx, id)
	})
}

func retryOnConflict(ctx context.Context, store Store, what string, mutate func(*Resource) error, get func(context.Context) (Resource, error)) (Resource, error) {
	var lastErr error
	for range maxConflictRetries {
		r, err := get(ctx)
		if err != nil {
			return Resource{}, err
		}
		if err := mutate(&r); err != nil {
			if errors.Is(err, ErrSkipUpdate) {
				return r, nil
			}
			return Resource{}, err
		}
		out, err := store.Put(ctx, r)
		if errors.Is(err, ErrConflict) {
			// Put with the read SyncVersion is a compare-and-set: a concurrent
			// writer landed in between, so yield to let it settle, then re-read
			// and reapply against the new version.
			lastErr = err
			runtime.Gosched()
			continue
		}
		return out, err
	}
	return Resource{}, fmt.Errorf("resource: update of %s gave up after %d conflicting writes: %w", what, maxConflictRetries, lastErr)
}
