package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/resource"
)

// Result reports what a Sync did, for logging and tests.
type Result struct {
	Created   int // bundled extensions newly added
	Updated   int // bundled extensions whose spec changed with the binary
	Unchanged int // bundled extensions already current
	Retired   int // bundled extensions removed from the catalog and deleted
	Forked    int // user-forked extensions left untouched
}

// Sync reconciles the embedded official catalog into the resource store. It is
// idempotent and preserves user intent:
//
//   - A new official extension is created, stamped as bundled.
//   - An official extension whose shipped spec changed is updated in place, carrying
//     its observed status (so a disabled extension stays disabled) forward.
//   - An official extension already current is left alone (no version churn).
//   - An extension the user forked is never overwritten.
//   - A bundled extension no longer in the catalog (removed in a new binary) is
//     retired.
//
// The store must admit the Extension kind (register extension.KindDef first).
func Sync(ctx context.Context, store resource.Store) (Result, error) {
	entries, err := Entries()
	if err != nil {
		return Result{}, err
	}
	var res Result
	inCatalog := make(map[string]bool, len(entries))

	for _, e := range entries {
		inCatalog[e.Name] = true
		existing, err := store.Get(ctx, extension.Kind, bundledScope, e.Name)
		switch {
		case errors.Is(err, resource.ErrNotFound):
			if _, err := store.Put(ctx, resourceFor(e)); err != nil {
				return res, fmt.Errorf("catalog: create %q: %w", e.Name, err)
			}
			res.Created++
		case err != nil:
			return res, fmt.Errorf("catalog: read %q: %w", e.Name, err)
		default:
			applied, err := reconcile(ctx, store, e, existing)
			if err != nil {
				return res, err
			}
			res.add(applied)
		}
	}

	retired, err := retireMissing(ctx, store, inCatalog)
	if err != nil {
		return res, err
	}
	res.Retired = retired
	return res, nil
}

// reconcile updates a bundled extension that already exists. A user fork is left
// untouched; an unchanged bundled spec is skipped; a changed one is re-applied with
// its status preserved.
func reconcile(ctx context.Context, store resource.Store, e Entry, existing resource.Resource) (Result, error) {
	if existing.Labels[SourceLabel] == SourceForked {
		return Result{Forked: 1}, nil
	}
	if sameSpec(existing.Spec, e.Raw) {
		return Result{Unchanged: 1}, nil
	}
	r := resourceFor(e)
	// Carry the observed status forward so re-syncing a changed spec does not flip a
	// user-disabled extension back on.
	r.Status = existing.Status
	if _, err := store.Put(ctx, r); err != nil {
		return Result{}, fmt.Errorf("catalog: update %q: %w", e.Name, err)
	}
	return Result{Updated: 1}, nil
}

// retireMissing deletes bundled extensions that are no longer in the catalog, so a
// spec removed in a new binary does not linger. A forked extension is never retired:
// the user owns it.
func retireMissing(ctx context.Context, store resource.Store, inCatalog map[string]bool) (int, error) {
	all, err := store.List(ctx, extension.Kind, bundledScope, nil)
	if err != nil {
		return 0, fmt.Errorf("catalog: list extensions: %w", err)
	}
	retired := 0
	for _, r := range all {
		if r.Labels[SourceLabel] != SourceBundled || inCatalog[r.Name] {
			continue
		}
		if err := store.Delete(ctx, extension.Kind, bundledScope, r.Name); err != nil {
			return retired, fmt.Errorf("catalog: retire %q: %w", r.Name, err)
		}
		retired++
	}
	return retired, nil
}

// Fork takes user ownership of a bundled extension by relabeling it forked, so a
// later Sync leaves it and the user's edits alone. It is a no-op on an extension
// that is not bundled.
func Fork(ctx context.Context, store resource.Store, name string) error {
	r, err := store.Get(ctx, extension.Kind, bundledScope, name)
	if err != nil {
		return err
	}
	if r.Labels[SourceLabel] != SourceBundled {
		return nil
	}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	r.Labels[SourceLabel] = SourceForked
	_, err = store.Put(ctx, r)
	return err
}

// sameSpec reports whether two spec encodings are semantically equal, comparing
// canonical JSON so a difference in key order or whitespace is not mistaken for a
// change.
func sameSpec(a, b json.RawMessage) bool {
	ca, oka := canonicalJSON(a)
	cb, okb := canonicalJSON(b)
	return oka && okb && ca == cb
}

func canonicalJSON(b json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func (r *Result) add(o Result) {
	r.Created += o.Created
	r.Updated += o.Updated
	r.Unchanged += o.Unchanged
	r.Retired += o.Retired
	r.Forked += o.Forked
}
