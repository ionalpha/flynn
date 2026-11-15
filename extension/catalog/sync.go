package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/resource"
)

// Result reports what a Sync did.
type Result struct {
	Created   int // bundled extensions newly added
	Updated   int // bundled extensions whose spec changed with the binary
	Unchanged int // bundled extensions already current
	Retired   int // bundled extensions removed from the catalog and deleted
	Forked    int // user-forked extensions left untouched
	Dev       int // dev-linked extensions left untouched
}

// Sync reconciles the embedded catalog into the store, idempotently. A changed
// spec keeps its status, so a disabled extension stays disabled; an unchanged
// one is not re-versioned. Forked and dev-linked extensions are never
// overwritten. Bundled extensions dropped from the catalog are retired. The
// store must admit [extension.KindDef].
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

func reconcile(ctx context.Context, store resource.Store, e Entry, existing resource.Resource) (Result, error) {
	switch existing.Labels[SourceLabel] {
	case SourceForked:
		return Result{Forked: 1}, nil
	case SourceDev:
		// Overwriting a dev link would swap the author's build for the release
		// unnoticed. The link stays unsigned, so keeping the name grants nothing.
		return Result{Dev: 1}, nil
	}
	if sameSpec(existing.Spec, e.Raw) {
		return Result{Unchanged: 1}, nil
	}
	r := resourceFor(e)
	// Keep status so a re-sync does not re-enable a disabled extension.
	r.Status = existing.Status
	if _, err := store.Put(ctx, r); err != nil {
		return Result{}, fmt.Errorf("catalog: update %q: %w", e.Name, err)
	}
	return Result{Updated: 1}, nil
}

// retireMissing deletes bundled extensions absent from the catalog. Forked ones
// belong to the user and stay.
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

// Fork relabels a bundled extension as forked so Sync leaves it alone. It is a
// no-op on one that is not bundled.
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

// sameSpec compares canonical JSON, ignoring key order and whitespace.
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
	r.Dev += o.Dev
}
