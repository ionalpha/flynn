package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/resource/resourcetest"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestResourceConformance holds the durable resource backend to the identical
// contract as the in-memory one: a single line proves byte-for-byte parity.
func TestResourceConformance(t *testing.T) {
	resourcetest.RunSuite(t, func(reg *resource.Registry) resource.Store {
		p, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			panic(err)
		}
		return p.Resources(reg)
	})
}

// TestResourcePersistsAcrossReopen is the point of a durable backend: a resource
// written by one process is read back by the next, from the same file.
func TestResourcePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "r.db")
	reg := resourcetest.NewRegistry(t)

	p1, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := p1.Resources(reg).Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "alpha",
		Labels: map[string]string{"tier": "pro"},
		Spec:   json.RawMessage(`{"size":"m"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p2.Close() }()
	got, err := p2.Resources(reg).Get(ctx, "Widget", resource.Scope{}, "alpha")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.ID != saved.ID || got.ContentHash != saved.ContentHash {
		t.Fatalf("reopened resource differs: got id=%q hash=%q, want id=%q hash=%q", got.ID, got.ContentHash, saved.ID, saved.ContentHash)
	}
	if got.Labels["tier"] != "pro" {
		t.Fatalf("labels did not survive reopen: %+v", got.Labels)
	}
}

// TestResourceRebuild proves the durable projection is derived: reprojecting the
// table from the log leaves it unchanged, and is idempotent.
func TestResourceRebuild(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	rs := p.Resources(reg)

	put := func(name, size string) {
		if _, err := rs.Put(ctx, resource.Resource{APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: name, Spec: json.RawMessage(`{"size":"` + size + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	put("a", "s")
	put("b", "m")
	put("a", "l") // update
	if err := rs.Delete(ctx, "Widget", resource.Scope{}, "b"); err != nil {
		t.Fatal(err)
	}

	before, _ := rs.List(ctx, "Widget", resource.Scope{}, nil)
	rebuilder, ok := rs.(interface{ Rebuild(context.Context) error })
	if !ok {
		t.Fatal("sqlite resource store should expose Rebuild")
	}
	if err := rebuilder.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after, _ := rs.List(ctx, "Widget", resource.Scope{}, nil)
	if len(before) != len(after) || len(after) != 1 || after[0].Name != "a" || after[0].ContentHash != before[0].ContentHash {
		t.Fatalf("rebuild changed the projection: before=%d after=%d", len(before), len(after))
	}
}
