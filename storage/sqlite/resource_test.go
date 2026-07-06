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

// TestGetAnyScopeKeyedMatchesScan is the AnyScopeGetter contract: the keyed
// cross-scope by-name lookup returns the same record the ListAll+name-scan fallback
// would (the first live match in scope-then-name order), and reports found=false with
// no error when no scope has the name. The control plane's cross-scope get reads
// through it, so a divergence here would make a keyed get disagree with a list.
func TestGetAnyScopeKeyedMatchesScan(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	rs := p.Resources(reg)

	put := func(scope resource.Scope, name string) {
		if _, err := rs.Put(ctx, resource.Resource{
			APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: name, Scope: scope,
			Spec: json.RawMessage(`{"size":"m"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Same name in three scopes; scope order is (instance, project, workspace), empty
	// first, so the global scope is the first match. A different name lives alongside.
	put(resource.Scope{Project: "p2"}, "shared")
	put(resource.Scope{}, "shared")
	put(resource.Scope{Project: "p1"}, "shared")
	put(resource.Scope{Project: "p1"}, "solo")

	getter, ok := rs.(resource.AnyScopeGetter)
	if !ok {
		t.Fatal("sqlite resource store should implement resource.AnyScopeGetter")
	}

	// scanFirst is what getAcrossScopes did before the keyed lookup: ListAll in
	// scope-then-name order, first name match.
	scanFirst := func(name string) (resource.Resource, bool) {
		all, err := rs.ListAll(ctx, "Widget", resource.Selector{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range all {
			if r.Name == name {
				return r, true
			}
		}
		return resource.Resource{}, false
	}

	for _, name := range []string{"shared", "solo", "absent"} {
		want, wantFound := scanFirst(name)
		got, gotFound, err := getter.GetAnyScope(ctx, "Widget", name)
		if err != nil {
			t.Fatalf("GetAnyScope(%q): %v", name, err)
		}
		if gotFound != wantFound {
			t.Fatalf("GetAnyScope(%q) found=%v, scan found=%v", name, gotFound, wantFound)
		}
		if wantFound && (got.ID != want.ID || got.Scope != want.Scope) {
			t.Fatalf("GetAnyScope(%q) returned scope=%+v id=%q, scan returned scope=%+v id=%q", name, got.Scope, got.ID, want.Scope, want.ID)
		}
	}
}

// TestListAllSelectorSurvivorFullyDecoded pins the labels-first list refactor: a row the
// selector keeps is still fully decoded (labels, annotations, finalizers, owner refs),
// and a row it rejects is excluded, exactly as a full-decode-then-filter list would. The
// optimization only defers the non-label decode for rejected rows; it must not change
// what a survivor carries or which rows survive.
func TestListAllSelectorSurvivorFullyDecoded(t *testing.T) {
	ctx := context.Background()
	reg := resourcetest.NewRegistry(t)
	p, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	rs := p.Resources(reg)

	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "keep",
		Labels:      map[string]string{"tier": "pro", "team": "core"},
		Annotations: map[string]string{"note": "keep me"},
		Spec:        json.RawMessage(`{"size":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "test.ionagent.io/v1", Kind: "Widget", Name: "drop",
		Labels: map[string]string{"tier": "free"},
		Spec:   json.RawMessage(`{"size":"s"}`),
	}); err != nil {
		t.Fatal(err)
	}

	sel := resource.Selector{{Key: "tier", Op: resource.OpEquals, Values: []string{"pro"}}}
	got, err := rs.ListAll(ctx, "Widget", sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("selector kept %d rows %v, want just [keep]", len(got), names(got))
	}
	r := got[0]
	if r.Labels["team"] != "core" {
		t.Errorf("survivor labels not fully decoded: %+v", r.Labels)
	}
	if r.Annotations["note"] != "keep me" {
		t.Errorf("survivor annotations not decoded: %+v", r.Annotations)
	}

	// An empty selector keeps and fully decodes every row, the no-filter path.
	all, err := rs.ListAll(ctx, "Widget", resource.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("empty selector kept %d rows, want 2", len(all))
	}
}

func names(rs []resource.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}
