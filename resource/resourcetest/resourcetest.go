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
	"time"

	"github.com/ionalpha/flynn/hlc"
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
	t.Run("Finalizers", func(t *testing.T) { testFinalizers(t, newStore) })
	t.Run("OwnerReferences", func(t *testing.T) { testOwnerReferences(t, newStore) })
	t.Run("ContentHash", func(t *testing.T) { testContentHash(t, newStore) })
	t.Run("Bitemporal", func(t *testing.T) { testBitemporal(t, newStore) })
	t.Run("MetaCircularKind", func(t *testing.T) { testMetaCircularKind(t, newStore) })
	t.Run("EventSourced", func(t *testing.T) { testEventSourced(t, newStore) })
	t.Run("Merge", func(t *testing.T) { testMerge(t, newStore) })
	t.Run("MergeConverges", func(t *testing.T) { testMergeConverges(t, newStore) })
	t.Run("MergeValidation", func(t *testing.T) { testMergeValidation(t, newStore) })
}

// remoteWidget builds a Widget as it would arrive replicated from another
// instance: a fully stamped record carrying its own envelope (a stable ID, an HLC,
// origin/writer and provenance), the shape Merge consumes. wall is the HLC wall
// time; later writes use a larger wall.
func remoteWidget(id, name, size string, wall int64, writer string, actor spine.ActorType) resource.Resource {
	r := resource.Resource{
		APIVersion: widgetAPIVersion,
		Kind:       widgetKind,
		ID:         id,
		Name:       name,
		Spec:       json.RawMessage(`{"size":"` + size + `"}`),
		Envelope: resource.Envelope{
			SyncVersion:      1,
			Version:          1,
			OriginInstanceID: writer,
			LastWriterID:     writer,
			WriterActor:      actor,
			UpdatedHLC:       hlc.Time{Wall: wall},
		},
	}
	if h, err := resource.Hash(r); err == nil {
		r.ContentHash = h
	}
	return r
}

// testMerge exercises the cross-instance apply path end to end: first-insert,
// last-writer-wins by HLC, idempotent re-apply, human-over-agent precedence,
// tombstone propagation and resurrection, and replay of the merged stream.
func testMerge(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	reg := NewRegistry(t)
	s := newStore(reg)
	defer func() { _ = s.Close() }()

	// Absent locally: the remote is inserted verbatim, envelope preserved.
	r1 := remoteWidget("rid-1", "alpha", "m", 1000, "B", spine.ActorAgent)
	res, err := s.Merge(ctx, r1)
	if err != nil || res.Outcome != resource.MergeApplied {
		t.Fatalf("first merge = (%v, %v), want applied", res.Outcome, err)
	}
	got, err := s.GetByID(ctx, "rid-1")
	if err != nil || got.OriginInstanceID != "B" || got.LastWriterID != "B" {
		t.Fatalf("inserted record lost its remote envelope: %+v (%v)", got.Envelope, err)
	}

	// Re-applying the same write is an idempotent no-op.
	if res, _ := s.Merge(ctx, r1); res.Outcome != resource.MergeUnchanged {
		t.Fatalf("re-merge of same write = %v, want unchanged", res.Outcome)
	}

	// A newer write (higher HLC) wins.
	r2 := remoteWidget("rid-1", "alpha", "l", 2000, "C", spine.ActorAgent)
	if res, _ := s.Merge(ctx, r2); res.Outcome != resource.MergeApplied {
		t.Fatalf("newer merge = %v, want applied", res.Outcome)
	}
	if got, _ := s.GetByID(ctx, "rid-1"); got.LastWriterID != "C" || specSize(got) != "l" {
		t.Fatalf("newer write did not win: writer=%q size=%q", got.LastWriterID, specSize(got))
	}

	// An older write (lower HLC) is ignored; local state is untouched.
	old := remoteWidget("rid-1", "alpha", "s", 500, "D", spine.ActorAgent)
	if res, _ := s.Merge(ctx, old); res.Outcome != resource.MergeIgnored {
		t.Fatalf("older merge = %v, want ignored", res.Outcome)
	}
	if got, _ := s.GetByID(ctx, "rid-1"); got.LastWriterID != "C" || specSize(got) != "l" {
		t.Fatal("older write must not change local state")
	}

	// Provenance precedence: a human write wins over an agent write even with a
	// LOWER clock, and a later agent write does not silently overwrite it.
	human := remoteWidget("rid-1", "alpha", "s", 100, "E", spine.ActorHuman)
	if res, _ := s.Merge(ctx, human); res.Outcome != resource.MergeApplied {
		t.Fatalf("human merge = %v, want applied (precedence over agent)", res.Outcome)
	}
	if got, _ := s.GetByID(ctx, "rid-1"); got.WriterActor != spine.ActorHuman || specSize(got) != "s" {
		t.Fatalf("human write did not win: actor=%q size=%q", got.WriterActor, specSize(got))
	}
	agentNewer := remoteWidget("rid-1", "alpha", "m", 9000, "F", spine.ActorAgent)
	if res, _ := s.Merge(ctx, agentNewer); res.Outcome != resource.MergeIgnored {
		t.Fatalf("agent-over-human merge = %v, want ignored", res.Outcome)
	}

	// Tombstone propagates: a delete with a higher HLC removes the record.
	del := remoteWidget("rid-1", "alpha", "s", 200, "E", spine.ActorHuman)
	del.Deleted = true
	if res, _ := s.Merge(ctx, del); res.Outcome != resource.MergeApplied {
		t.Fatalf("tombstone merge = %v, want applied", res.Outcome)
	}
	if _, err := s.GetByID(ctx, "rid-1"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("merged tombstone still visible: %v", err)
	}
	// A later write after the tombstone intentionally resurrects.
	revive := remoteWidget("rid-1", "alpha", "l", 300, "E", spine.ActorHuman)
	if res, _ := s.Merge(ctx, revive); res.Outcome != resource.MergeApplied {
		t.Fatalf("resurrection merge = %v, want applied", res.Outcome)
	}
	if got, err := s.GetByID(ctx, "rid-1"); err != nil || got.Deleted || specSize(got) != "l" {
		t.Fatalf("resurrection failed: %+v (%v)", got, err)
	}

	// The merged stream is authoritative: a store folded purely from it matches.
	if el, ok := s.(eventLogged); ok {
		replayed, err := resource.Replay(ctx, el.Log(), reg)
		if err != nil {
			t.Fatal(err)
		}
		live, _ := s.GetByID(ctx, "rid-1")
		rep, err := replayed.GetByID(ctx, "rid-1")
		if err != nil || rep.ContentHash != live.ContentHash || rep.LastWriterID != live.LastWriterID {
			t.Fatalf("merged stream did not replay identically: live=%+v rep=%+v (%v)", live.Envelope, rep.Envelope, err)
		}
	}
}

// testMergeConverges asserts the merge is order-independent: two instances that
// receive the same pair of conflicting writes in opposite orders end identical.
func testMergeConverges(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	a := newStore(NewRegistry(t))
	b := newStore(NewRegistry(t))
	defer func() { _ = a.Close(); _ = b.Close() }()

	x := remoteWidget("rid", "w", "s", 1000, "X", spine.ActorAgent)
	y := remoteWidget("rid", "w", "l", 1000, "Y", spine.ActorAgent) // same HLC, writer tiebreak

	mustMerge(t, a, x)
	mustMerge(t, a, y)
	mustMerge(t, b, y)
	mustMerge(t, b, x)

	ra, _ := a.GetByID(ctx, "rid")
	rb, _ := b.GetByID(ctx, "rid")
	if ra.ContentHash != rb.ContentHash || ra.LastWriterID != rb.LastWriterID {
		t.Fatalf("merge not convergent: a=%q/%q b=%q/%q", ra.LastWriterID, ra.ContentHash, rb.LastWriterID, rb.ContentHash)
	}
	if ra.LastWriterID != "Y" { // equal HLC, higher writer id wins deterministically
		t.Fatalf("tiebreak winner = %q, want Y", ra.LastWriterID)
	}
}

func testMergeValidation(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	// A record without the envelope Merge relies on is rejected, not half-applied.
	noID := remoteWidget("", "w", "s", 1, "B", spine.ActorAgent)
	if _, err := s.Merge(ctx, noID); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("merge without ID = %v, want ErrInvalid", err)
	}
	noHLC := remoteWidget("rid", "w", "s", 0, "B", spine.ActorAgent)
	if _, err := s.Merge(ctx, noHLC); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("merge without HLC = %v, want ErrInvalid", err)
	}
	// A replicated record of an unregistered kind is rejected (admission still runs).
	unknown := remoteWidget("rid", "w", "s", 1, "B", spine.ActorAgent)
	unknown.Kind = "Nope"
	unknown.APIVersion = "x/v1"
	if _, err := s.Merge(ctx, unknown); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("merge of unregistered kind = %v, want ErrInvalid", err)
	}
}

func specSize(r resource.Resource) string {
	var m struct {
		Size string `json:"size"`
	}
	_ = json.Unmarshal(r.Spec, &m)
	return m.Size
}

func mustMerge(t *testing.T, s resource.Store, r resource.Resource) resource.MergeResult {
	t.Helper()
	res, err := s.Merge(context.Background(), r)
	if err != nil {
		t.Fatalf("merge %s: %v", r.ID, err)
	}
	return res
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

// testFinalizers covers the deletion lifecycle: a resource with finalizers is not
// removed by Delete but marked terminating and kept live, re-delete is idempotent,
// and the deletion completes only when the last finalizer is cleared via Put.
func testFinalizers(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	withFinalizers := func(name string, fz ...string) resource.Resource {
		r := widget(name, "m", nil)
		r.Finalizers = fz
		return r
	}

	created := mustPut(t, s, withFinalizers("g", "worktree", "children"))
	if created.DeletionTimestamp != nil {
		t.Fatal("a fresh resource must not be terminating")
	}

	// Delete does not remove a finalized resource; it marks it terminating and
	// leaves it live and visible to controllers.
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "g"); err != nil {
		t.Fatalf("delete with finalizers: %v", err)
	}
	term, err := s.Get(ctx, widgetKind, resource.Scope{}, "g")
	if err != nil {
		t.Fatalf("a terminating resource must still be readable: %v", err)
	}
	if term.DeletionTimestamp == nil {
		t.Fatal("delete did not set DeletionTimestamp")
	}
	if len(term.Finalizers) != 2 {
		t.Fatalf("delete must not drop finalizers: %v", term.Finalizers)
	}
	if l, _ := s.List(ctx, widgetKind, resource.Scope{}, nil); len(l) != 1 {
		t.Fatalf("terminating resource missing from List: %d", len(l))
	}

	// Re-deleting an already-terminating resource is an idempotent no-op.
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "g"); err != nil {
		t.Fatalf("re-delete of terminating resource = %v, want nil (idempotent)", err)
	}

	// Removing one of two finalizers keeps it terminating, not yet gone.
	term.Finalizers = []string{"children"}
	one, err := s.Put(ctx, term)
	if err != nil {
		t.Fatalf("remove one finalizer: %v", err)
	}
	if one.Deleted || one.DeletionTimestamp == nil {
		t.Fatalf("removing one of two finalizers must keep it terminating: %+v", one.Envelope)
	}
	if _, err := s.Get(ctx, widgetKind, resource.Scope{}, "g"); err != nil {
		t.Fatalf("still-finalized resource must be readable: %v", err)
	}

	// Removing the last finalizer completes the deletion.
	one.Finalizers = nil
	gone, err := s.Put(ctx, one)
	if err != nil {
		t.Fatalf("remove last finalizer: %v", err)
	}
	if !gone.Deleted {
		t.Fatal("clearing the last finalizer must complete deletion")
	}
	if _, err := s.Get(ctx, widgetKind, resource.Scope{}, "g"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("completed deletion still visible: %v", err)
	}

	// A resource with no finalizers tombstones immediately (the classic path).
	mustPut(t, s, widget("plain", "s", nil))
	if err := s.Delete(ctx, widgetKind, resource.Scope{}, "plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, widgetKind, resource.Scope{}, "plain"); !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("unfinalized delete should be immediate: %v", err)
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

// widgetValid is a widget carrying an explicit valid-time window.
func widgetValid(name, size string, from, to *time.Time) resource.Resource {
	r := widget(name, size, nil)
	r.ValidFrom = from
	r.ValidTo = to
	return r
}

// testBitemporal proves the valid-time axis is wired end to end through a backend:
// the envelope fields round-trip (including the SQLite string encoding), the nil
// default means valid from creation onward, ValidAt honours the half-open window,
// and valid-time participates in the content hash.
func testBitemporal(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// No valid-time set: round-trips as nil/nil and is valid from creation onward.
	def := mustPut(t, s, widget("plain", "m", nil))
	if def.ValidFrom != nil || def.ValidTo != nil {
		t.Fatalf("default valid-time should be nil/nil, got %v/%v", def.ValidFrom, def.ValidTo)
	}
	gotDef, err := s.Get(ctx, widgetKind, resource.Scope{}, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if gotDef.ValidFrom != nil || gotDef.ValidTo != nil {
		t.Fatalf("default valid-time did not round-trip as nil: %v/%v", gotDef.ValidFrom, gotDef.ValidTo)
	}
	if !gotDef.ValidAt(gotDef.CreatedAt) || gotDef.ValidAt(gotDef.CreatedAt.Add(-time.Hour)) {
		t.Fatal("nil valid-time should be valid from creation onward, not before")
	}

	// Explicit valid-time round-trips exactly, and ValidAt honours the window.
	mustPut(t, s, widgetValid("dated", "m", &from, &to))
	got, err := s.Get(ctx, widgetKind, resource.Scope{}, "dated")
	if err != nil {
		t.Fatal(err)
	}
	if got.ValidFrom == nil || !got.ValidFrom.Equal(from) || got.ValidTo == nil || !got.ValidTo.Equal(to) {
		t.Fatalf("valid-time did not round-trip: got %v/%v want %v/%v", got.ValidFrom, got.ValidTo, from, to)
	}
	if got.ValidAt(from.Add(-time.Hour)) || !got.ValidAt(from) || !got.ValidAt(from.Add(24*time.Hour)) || got.ValidAt(to) {
		t.Fatal("ValidAt did not honour the round-tripped [from, to) window")
	}

	// Valid-time is part of the content hash: changing only the window changes the
	// hash, and re-putting the same window is idempotent.
	h0 := mustPut(t, s, widget("h", "m", nil))
	h1 := mustPut(t, s, widgetValid("h", "m", &from, &to))
	if h1.ContentHash == h0.ContentHash {
		t.Fatal("adding valid-time did not change the content hash")
	}
	h2 := mustPut(t, s, widgetValid("h", "m", &from, &to))
	if h2.ContentHash != h1.ContentHash {
		t.Fatal("identical valid-time produced a different content hash")
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

// testOwnerReferences asserts the ownership graph edge round-trips through any
// backend: a resource's owner references and its single controller owner survive a
// write and read back intact, and a resource with no owners has no controller.
func testOwnerReferences(t *testing.T, newStore func(*resource.Registry) resource.Store) {
	ctx := context.Background()
	s := newStore(NewRegistry(t))
	defer func() { _ = s.Close() }()

	owned := widget("child", "m", nil)
	owned.OwnerReferences = []resource.OwnerReference{
		{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "parent", ID: "parent-id", Controller: true},
		{APIVersion: widgetAPIVersion, Kind: widgetKind, Name: "sidecar", ID: "sidecar-id"},
	}
	mustPut(t, s, owned)

	got, err := s.Get(ctx, widgetKind, resource.Scope{}, "child")
	if err != nil {
		t.Fatalf("get owned: %v", err)
	}
	if len(got.OwnerReferences) != 2 {
		t.Fatalf("owner references not round-tripped: %v", got.OwnerReferences)
	}
	ctrl, ok := got.Controller()
	if !ok || ctrl.ID != "parent-id" || !ctrl.Controller {
		t.Fatalf("controller owner not preserved: %+v ok=%v", ctrl, ok)
	}

	root := mustPut(t, s, widget("root", "m", nil))
	if _, ok := root.Controller(); ok {
		t.Fatal("a resource with no owner references must have no controller")
	}
}
