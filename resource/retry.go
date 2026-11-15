package resource

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// maxConflictRetries bounds Update's retry loop. A retry is spent only when another
// writer landed in between, and contention is a handful of writers, so the bound
// stops a writer that never stops.
const maxConflictRetries = 1000

// ErrSkipUpdate is returned by a mutate function to end Update without writing because
// the observed state makes the write unnecessary. Update returns the resource as read
// and nil.
var ErrSkipUpdate = errors.New("resource: skip update")

// Update applies mutate to the resource at (kind, scope, name) under optimistic
// concurrency, re-reading and reapplying on ErrConflict. It is the one
// read-modify-write retry policy, so every caller converges the same way. mutate
// gets a fresh copy each attempt and must be safe to rerun. Read errors, mutate
// errors other than ErrSkipUpdate and non-conflict write errors return as-is; an
// exhausted budget returns an error matching ErrConflict.
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
			// A concurrent writer landed; yield so it settles, then re-read.
			lastErr = err
			runtime.Gosched()
			continue
		}
		return out, err
	}
	return Resource{}, fmt.Errorf("resource: update of %s gave up after %d conflicting writes: %w", what, maxConflictRetries, lastErr)
}
