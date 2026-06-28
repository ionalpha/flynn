package catalog

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/resource"
)

// TestPropSyncConverges asserts that however many times Sync runs, the store ends in
// the same state: exactly the catalog's bundled extensions, and every sync after the
// first reports no changes. This is the level-triggered guarantee a startup sync
// relies on.
func TestPropSyncConverges(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		store := newStore(t)
		ctx := context.Background()
		runs := rapid.IntRange(1, 6).Draw(rt, "runs")

		entries, err := Entries()
		if err != nil {
			rt.Fatalf("entries: %v", err)
		}
		for i := range runs {
			res, err := Sync(ctx, store)
			if err != nil {
				rt.Fatalf("sync %d: %v", i, err)
			}
			if i == 0 {
				if res.Created != len(entries) {
					rt.Fatalf("first sync should create all: %+v", res)
				}
			} else if res.Created != 0 || res.Updated != 0 || res.Retired != 0 {
				rt.Fatalf("sync %d should be a no-op: %+v", i, res)
			}
		}

		// The store holds exactly the catalog, each labelled bundled.
		all, err := store.List(ctx, extension.Kind, resource.Scope{}, nil)
		if err != nil {
			rt.Fatalf("list: %v", err)
		}
		if len(all) != len(entries) {
			rt.Fatalf("expected %d extensions, got %d", len(entries), len(all))
		}
		for _, r := range all {
			if r.Labels[SourceLabel] != SourceBundled {
				rt.Fatalf("extension %q not labelled bundled", r.Name)
			}
		}
	})
}
