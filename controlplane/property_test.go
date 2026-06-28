package controlplane_test

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/controlplane"
)

// TestProp_ListAndWatchReflectStore checks that, for any number of resources, a
// List returns exactly one row per resource ordered by name, and a fresh
// PollWatcher reports each current resource once as Added and then nothing on a
// steady poll. Resources are inserted in reverse order to exercise the ordering.
func TestProp_ListAndWatchReflectStore(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := newStore(t)
		ctx := context.Background()

		n := rapid.IntRange(0, 8).Draw(rt, "n")
		for i := n - 1; i >= 0; i-- {
			putWidget(t, store, fmt.Sprintf("w%02d", i), "c", "idle")
		}

		table, err := controlplane.List(ctx, store, descriptor(), nil)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		if len(table.Rows) != n {
			rt.Fatalf("rows = %d, want %d", len(table.Rows), n)
		}
		for i, row := range table.Rows {
			want := fmt.Sprintf("w%02d", i)
			if row.Name != want || row.Cells[0] != want {
				rt.Fatalf("row %d = %q/%q, want %q (rows must be name-ordered)", i, row.Name, row.Cells[0], want)
			}
		}

		w := controlplane.NewPollWatcher(store, widgetKind, nil)
		changes, err := w.Poll(ctx)
		if err != nil {
			rt.Fatalf("poll: %v", err)
		}
		if len(changes) != n {
			rt.Fatalf("first poll changes = %d, want %d", len(changes), n)
		}
		for _, c := range changes {
			if c.Kind != controlplane.Added {
				rt.Fatalf("change kind = %q, want Added", c.Kind)
			}
		}
		if again, err := w.Poll(ctx); err != nil || len(again) != 0 {
			rt.Fatalf("steady poll = %v, %v; want no changes", again, err)
		}
	})
}
