package controlplane_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/resource"
)

func TestPollWatcherReportsAddModifyDelete(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := controlplane.NewPollWatcher(store, widgetKind, nil)

	// First poll over an empty store: no changes.
	if changes, err := w.Poll(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("initial poll = %v, %v; want no changes", changes, err)
	}

	// Two new resources appear as Added, ordered by name.
	putWidget(t, store, "beta", "blue", "idle")
	putWidget(t, store, "alpha", "red", "idle")
	changes, err := w.Poll(ctx)
	if err != nil {
		t.Fatalf("poll after add: %v", err)
	}
	if len(changes) != 2 || changes[0].Kind != controlplane.Added || changes[1].Kind != controlplane.Added {
		t.Fatalf("add poll = %+v, want two Added", changes)
	}
	if changes[0].Resource.Name != "alpha" || changes[1].Resource.Name != "beta" {
		t.Fatalf("changes not ordered by name: %s, %s", changes[0].Resource.Name, changes[1].Resource.Name)
	}

	// No further change: a steady poll is empty.
	if changes, err := w.Poll(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("steady poll = %v, %v; want no changes", changes, err)
	}

	// Updating one resource reports exactly one Modified.
	putWidget(t, store, "alpha", "crimson", "working")
	changes, err = w.Poll(ctx)
	if err != nil {
		t.Fatalf("poll after modify: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != controlplane.Modified || changes[0].Resource.Name != "alpha" {
		t.Fatalf("modify poll = %+v, want one Modified for alpha", changes)
	}

	// Deleting a resource reports exactly one Deleted.
	if err := store.Delete(ctx, widgetKind, resource.Scope{}, "beta"); err != nil {
		t.Fatalf("delete beta: %v", err)
	}
	changes, err = w.Poll(ctx)
	if err != nil {
		t.Fatalf("poll after delete: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != controlplane.Deleted || changes[0].Resource.Name != "beta" {
		t.Fatalf("delete poll = %+v, want one Deleted for beta", changes)
	}
}
