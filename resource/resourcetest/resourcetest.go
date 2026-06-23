// Package resourcetest is the conformance suite for resource.Store. Every backend
// (the in-memory default, SQLite, a host's) runs RunSuite and must behave
// identically, so durable backends are held to byte-for-byte the same contract as
// the reference in-memory one rather than re-tested by hand.
package resourcetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// Test kind used across the suite: a Widget with a small schema (a required enum
// string and an optional non-negative integer).
const (
	widgetAPIVersion = "test.ionagent.io/v1"
	widgetKind       = "Widget"
)

var widgetSchema = json.RawMessage(`{
  "type": "object",
  "required": ["size"],
  "properties": {
    "size": {"type": "string", "enum": ["s", "m", "l"]},
    "count": {"type": "integer", "minimum": 0}
  },
  "additionalProperties": false
}`)

// NewRegistry returns a registry with the substrate's core kinds and the suite's
// Widget kind registered, so a backend factory can be built against it.
func NewRegistry(t *testing.T) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := reg.Register(resource.Kind{APIVersion: widgetAPIVersion, Name: widgetKind, Schema: widgetSchema}); err != nil {
		t.Fatalf("register widget kind: %v", err)
	}
	return reg
}

func widget(name, size string, labels map[string]string) resource.Resource {
	return resource.Resource{
		APIVersion: widgetAPIVersion,
		Kind:       widgetKind,
		Name:       name,
		Labels:     labels,
		Spec:       json.RawMessage(`{"size":"` + size + `"}`),
	}
}

// RunSuite runs the full resource.Store contract against stores built by newStore,
// which is handed the registry the suite registered its kinds in. Each subtest
// gets a fresh store.
func RunSuite(t *testing.T, newStore func(reg *resource.Registry) resource.Store) {
	t.Helper()
	t.Run("PutGetUpdate", func(t *testing.T) { testPutGetUpdate(t, newStore) })
	t.Run("Admission", func(t *testing.T) { testAdmission(t, newStore) })
	t.Run("OptimisticConcurrency", func(t *testing.T) { testCAS(t, newStore) })
	t.Run("ListSelector", func(t *testing.T) { testListSelector(t, newStore) })
	t.Run("ListAllScopes", func(t *testing.T) { testListAll(t, newStore) })
	t.Run("GenerateName", func(t *testing.T) { testGenerateName(t, newStore) })
	t.Run("Tombstone", func(t *testing.T) { testTombstone(t, newStore) })
	t.Run("ContentHash", func(t *testing.T) { testContentHash(t, newStore) })
	t.Run("MetaCircularKind", func(t *testing.T) { testMetaCircularKind(t, newStore) })
	t.Run("EventSourced", func(t *testing.T) { testEventSourced(t, newStore) })
}

func testPutGetUpdate(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	if _, err := s.Get(ctx, widgetKind, resource.Scope{}, "missing"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}

	a, err := s.Put(ctx, widget("alpha", "m", nil))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Version != 1 || a.SyncVersion != 1 || a.ContentHash == "" {
		t.Fatalf("create envelope wrong: %+v", a.Envelope)
	}
	if a.OriginInstanceID == "" || a.LastWriterID != a.OriginInstanceID {
		t.Fatalf("create origin/writer wrong: %+v", a.Envelope)
	}

	got, err := s.Get(ctx, widgetKind, resource.Scope{}, "alpha")
	if err != nil || got.ID != a.ID {
		t.Fatalf("Get = (%q, %v)", got.ID, err)
	}
	byID, err := s.GetByID(ctx, a.ID)
	if err != nil || byID.Name != "alpha" {
		t.Fatalf("GetByID = (%q, %v)", byID.Name, err)
	}

	b, err := s.Put(ctx, widget("alpha", "l", nil))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Version != 2 || b.SyncVersion != 2 || !b.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("update did not preserve id/created or bump versions: %+v", b)
	}
	if !a.UpdatedHLC.Before(b.UpdatedHLC) {
		t.Fatal("update did not advance the HLC")
	}
}

func testAdmission(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	// Unregistered kind.
	if _, err := s.Put(ctx, resource.Resource{APIVersion: "x/v1", Kind: "Nope", Name: "n", Spec: json.RawMessage(`{}`)}); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("unregistered kind = %v, want ErrInvalid", err)
	}
	// Missing required field.
	if _, err := s.Put(ctx, resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "x", Spec: json.RawMessage(`{}`)}); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("missing required = %v, want ErrInvalid", err)
	}
	// Enum violation.
	if _, err := s.Put(ctx, widget("x", "xl", nil)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("enum violation = %v, want ErrInvalid", err)
	}
	// Additional property rejected.
	bad := resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "x", Spec: json.RawMessage(`{"size":"s","extra":true}`)}
	if _, err := s.Put(ctx, bad); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("additionalProperties = %v, want ErrInvalid", err)
	}
	// Missing identity fields.
	if _, err := s.Put(ctx, resource.Resource{Kind: widgetKind, Name: "x"}); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("missing APIVersion = %v, want ErrInvalid", err)
	}
}

func testCAS(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	created, _ := s.Put(ctx, widget("a", "s", nil))
	upd := created
	upd.Spec = json.RawMessage(`{"size":"m"}`)
	saved, err := s.Put(ctx, upd) // carries SyncVersion 1, matches
	if err != nil || saved.SyncVersion != 2 {
		t.Fatalf("matching-version update = (%d, %v)", saved.SyncVersion, err)
	}
	upd.Spec = json.RawMessage(`{"size":"l"}`) // still carries stale SyncVersion 1
	if _, err := s.Put(ctx, upd); !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	// Create-with-version (no existing record) is a conflict.
	ghost := widget("ghost", "s", nil)
	ghost.SyncVersion = 7
	if _, err := s.Put(ctx, ghost); !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("create-with-version = %v, want ErrConflict", err)
	}
}

func testListSelector(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	mustPut(t, s, widget("a", "s", map[string]string{"tier": "free"}))
	mustPut(t, s, widget("b", "m", map[string]string{"tier": "pro"}))
	mustPut(t, s, widget("c", "l", map[string]string{"tier": "pro"}))

	all, err := s.List(ctx, widgetKind, resource.Scope{}, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("List all = (%d, %v), want 3", len(all), err)
	}
	if all[0].Name != "a" || all[2].Name != "c" {
		t.Fatalf("List not ordered by name: %v", names(all))
	}

	sel, err := resource.ParseSelector("tier=pro")
	if err != nil {
		t.Fatal(err)
	}
	pro, err := s.List(ctx, widgetKind, resource.Scope{}, sel)
	if err != nil || len(pro) != 2 {
		t.Fatalf("List tier=pro = (%d, %v), want 2", len(pro), err)
	}

	sel2, _ := resource.ParseSelector("tier in (free), !archived")
	free, _ := s.List(ctx, widgetKind, resource.Scope{}, sel2)
	if len(free) != 1 || free[0].Name != "a" {
		t.Fatalf("List tier in (free),!archived = %v, want [a]", names(free))
	}

	// Scope isolation.
	mustPut(t, s, resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "a", Scope: resource.Scope{Project: "p"}, Spec: json.RawMessage(`{"size":"s"}`)})
	scoped, _ := s.List(ctx, widgetKind, resource.Scope{Project: "p"}, nil)
	if len(scoped) != 1 {
		t.Fatalf("scoped List = %d, want 1", len(scoped))
	}
}

func testListAll(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	global := widget("a", "s", map[string]string{"tier": "pro"})
	proj := resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "a", Scope: resource.Scope{Project: "p"}, Spec: json.RawMessage(`{"size":"m"}`)}
	work := resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "b", Scope: resource.Scope{Project: "p", Workspace: "w"}, Labels: map[string]string{"tier": "free"}, Spec: json.RawMessage(`{"size":"l"}`)}
	mustPut(t, s, global)
	mustPut(t, s, proj)
	mustPut(t, s, work)

	all, err := s.ListAll(ctx, widgetKind, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAll = (%d, %v), want 3 across scopes", len(all), err)
	}
	// Ordered by scope (instance, project, workspace) then name: global "a", then
	// project-p "a", then project-p/workspace-w "b".
	if all[0].Scope != (resource.Scope{}) || all[1].Scope.Project != "p" || all[1].Scope.Workspace != "" || all[2].Scope.Workspace != "w" {
		t.Fatalf("ListAll not ordered by scope: %v", scopes(all))
	}

	// A scoped List sees only its own namespace; ListAll spans them.
	if one, _ := s.List(ctx, widgetKind, resource.Scope{Project: "p"}, nil); len(one) != 1 {
		t.Fatalf("scoped List = %d, want 1", len(one))
	}

	// The selector applies across every scope.
	sel, _ := resource.ParseSelector("tier=pro")
	pro, err := s.ListAll(ctx, widgetKind, sel)
	if err != nil || len(pro) != 1 || pro[0].Scope != (resource.Scope{}) {
		t.Fatalf("ListAll tier=pro = (%d, %v), want only the global widget", len(pro), err)
	}

	// A tombstone drops out of ListAll.
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "a"); err != nil {
		t.Fatal(err)
	}
	if rest, _ := s.ListAll(ctx, widgetKind, nil); len(rest) != 2 {
		t.Fatalf("ListAll after delete = %d, want 2", len(rest))
	}
}

// testGenerateName covers server-assigned names for kinds with no natural name:
// each put is a fresh create named GenerateName + ID, an explicit Name still wins
// and upserts in place, and a resource with neither is rejected.
func testGenerateName(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	anon := func() resource.Resource {
		return resource.Resource{
			APIVersion:   widgetAPIVersion,
			Kind:         widgetKind,
			GenerateName: "w-",
			Spec:         json.RawMessage(`{"size":"s"}`),
		}
	}

	a, err := s.Put(ctx, anon())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Name != "w-"+a.ID || a.Version != 1 {
		t.Fatalf("generateName create = %+v, want Name = w-<id>", a)
	}
	if a.GenerateName != "" {
		t.Fatalf("GenerateName must be consumed on write, got %q", a.GenerateName)
	}
	// The assigned name is a first-class address, alongside the id.
	if got, err := s.Get(ctx, widgetKind, resource.Scope{}, a.Name); err != nil || got.ID != a.ID {
		t.Fatalf("Get(assigned name) = (%q, %v)", got.ID, err)
	}
	if got, err := s.GetByID(ctx, a.ID); err != nil || got.Name != a.Name {
		t.Fatalf("GetByID = (%q, %v)", got.Name, err)
	}

	// Each generateName put is a distinct create, never an update of the first.
	b, err := s.Put(ctx, anon())
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID || b.Name == a.Name || b.Version != 1 {
		t.Fatalf("second generateName put must be a distinct create: a=%s b=%s", a.Name, b.Name)
	}
	if all, _ := s.List(ctx, widgetKind, resource.Scope{}, nil); len(all) != 2 {
		t.Fatalf("List = %d, want 2 distinct generated records", len(all))
	}

	// An explicit Name takes precedence and upserts in place over repeated puts.
	named := anon()
	named.Name = "fixed"
	n, err := s.Put(ctx, named)
	if err != nil || n.Name != "fixed" {
		t.Fatalf("explicit Name should win: (%q, %v)", n.Name, err)
	}
	named.Spec = json.RawMessage(`{"size":"m"}`)
	if n2, err := s.Put(ctx, named); err != nil || n2.ID != n.ID || n2.Version != 2 {
		t.Fatalf("named re-put should update in place: %+v (%v)", n2, err)
	}

	// Neither Name nor GenerateName is an admission failure.
	if _, err := s.Put(ctx, resource.Resource{APIVersion: widgetAPIVersion, Kind: widgetKind, Spec: json.RawMessage(`{"size":"s"}`)}); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("no name/generateName = %v, want ErrInvalid", err)
	}
}

func testTombstone(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	orig := mustPut(t, s, widget("a", "s", nil))
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, widgetKind, resource.Scope{}, "a"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if l, _ := s.List(ctx, widgetKind, resource.Scope{}, nil); len(l) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(l))
	}
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "a"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
	// A put over the tombstone resurrects it with a newer HLC.
	revived := mustPut(t, s, widget("a", "l", nil))
	if revived.Deleted {
		t.Fatal("resurrected resource still tombstoned")
	}
	if !orig.UpdatedHLC.Before(revived.UpdatedHLC) {
		t.Fatal("resurrection must carry a newer HLC")
	}
}

func testContentHash(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	a := mustPut(t, s, widget("a", "m", map[string]string{"k": "v"}))
	// Re-putting identical content yields the same content hash (Merkle property),
	// even though the sync/content versions advance.
	b := mustPut(t, s, widget("a", "m", map[string]string{"k": "v"}))
	if a.ContentHash != b.ContentHash {
		t.Fatalf("identical content produced different hashes: %q vs %q", a.ContentHash, b.ContentHash)
	}
	if b.Version <= a.Version {
		t.Fatalf("re-put did not advance version: %d -> %d", a.Version, b.Version)
	}
	// Different content yields a different hash.
	c := mustPut(t, s, widget("a", "l", map[string]string{"k": "v"}))
	if c.ContentHash == a.ContentHash {
		t.Fatal("different content produced the same hash")
	}
}

func testMetaCircularKind(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	// A Kind is a Resource: store a kind definition through the same store.
	kindRes, err := resource.KindResource(resource.Kind{APIVersion: "skill.ionagent.io/v1", Name: "Skill"}, resource.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Put(ctx, kindRes)
	if err != nil {
		t.Fatalf("put Kind resource: %v", err)
	}
	if saved.Kind != resource.KindKind {
		t.Fatalf("stored kind = %q, want %q", saved.Kind, resource.KindKind)
	}
	got, err := s.Get(ctx, resource.KindKind, resource.Scope{}, "Skill")
	if err != nil {
		t.Fatalf("get Kind resource: %v", err)
	}
	if got.ID != saved.ID {
		t.Fatal("round-tripped Kind resource id mismatch")
	}
	// A Kind spec missing required fields is rejected by the Kind schema.
	bad := resource.Resource{APIVersion: resource.CoreGroupVersion, Kind: resource.KindKind, Name: "Broken", Spec: json.RawMessage(`{"name":"Broken"}`)}
	if _, err := s.Put(ctx, bad); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("invalid Kind spec = %v, want ErrInvalid", err)
	}
}

// eventLogged is the optional capability of a store that exposes its spine.
type eventLogged interface {
	Log() spine.Log
}

func testEventSourced(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	reg := NewRegistry(t)
	s := newStore(reg)
	defer func() { _ = s.Close() }()

	el, ok := s.(eventLogged)
	if !ok {
		t.Skip("store does not expose an event log")
	}

	mustPut(t, s, widget("a", "s", nil))
	mustPut(t, s, widget("b", "m", nil))
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "a"); err != nil {
		t.Fatal(err)
	}

	events, err := el.Log().Read(ctx, spine.Query{Stream: resource.ResourceStream})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 { // 2 puts + 1 delete
		t.Fatalf("resource stream has %d events, want 3 (a write bypassed the log?)", len(events))
	}

	// The log is authoritative: a store folded purely from it reproduces the reads.
	replayed, err := resource.Replay(ctx, el.Log(), reg)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := s.List(ctx, widgetKind, resource.Scope{}, nil)
	rep, _ := replayed.List(ctx, widgetKind, resource.Scope{}, nil)
	if len(live) != len(rep) {
		t.Fatalf("replayed list len %d != live %d", len(rep), len(live))
	}
	for i := range live {
		if live[i].ID != rep[i].ID || live[i].ContentHash != rep[i].ContentHash {
			t.Fatalf("replayed resource %d differs from live", i)
		}
	}
}

func mustPut(t *testing.T, s resource.Store, r resource.Resource) resource.Resource {
	t.Helper()
	out, err := s.Put(context.Background(), r)
	if err != nil {
		t.Fatalf("put %s/%s: %v", r.Kind, r.Name, err)
	}
	return out
}

func names(rs []resource.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

func scopes(rs []resource.Resource) []resource.Scope {
	out := make([]resource.Scope, len(rs))
	for i, r := range rs {
		out[i] = r.Scope
	}
	return out
}
