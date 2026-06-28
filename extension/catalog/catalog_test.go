package catalog

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/resource"
)

func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

func TestEntriesLoad(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one bundled spec")
	}
	for _, e := range entries {
		if e.Name == "" || len(e.Raw) == 0 {
			t.Fatalf("malformed entry: %+v", e)
		}
	}
}

// TestSyncAdmitsEverySpec proves every bundled spec is admitted by the Extension
// kind: a sync into a kind-checking store succeeds only if each spec is valid.
func TestSyncAdmitsEverySpec(t *testing.T) {
	store := newStore(t)
	res, err := Sync(context.Background(), store)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	entries, _ := Entries()
	if res.Created != len(entries) {
		t.Fatalf("expected %d created, got %+v", len(entries), res)
	}
}

// TestSyncIdempotent proves a second sync is a no-op: nothing is created, updated, or
// retired, so re-running at every startup does not churn the store.
func TestSyncIdempotent(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := Sync(ctx, store); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	res, err := Sync(ctx, store)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Created != 0 || res.Updated != 0 || res.Retired != 0 {
		t.Fatalf("second sync should be a no-op, got %+v", res)
	}
	entries, _ := Entries()
	if res.Unchanged != len(entries) {
		t.Fatalf("expected all unchanged, got %+v", res)
	}
}

// TestSyncUpdatesAndPreservesStatus proves a changed shipped spec is updated in
// place while a user-disabled status is carried forward.
func TestSyncUpdatesAndPreservesStatus(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	status, _ := extension.Status{Enabled: false}.Encode()
	// Seed a stale, disabled bundled version of an existing catalog entry.
	name := firstEntryName(t)
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       name,
		Labels:     map[string]string{SourceLabel: SourceBundled},
		Spec:       json.RawMessage(`{"baseURL":"https://stale.example.com"}`),
		Status:     status,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := Sync(ctx, store)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Updated < 1 {
		t.Fatalf("expected an update, got %+v", res)
	}
	got, err := store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := extension.DecodeSpec(got)
	if spec.BaseURL == "https://stale.example.com" {
		t.Fatal("spec was not updated to the shipped version")
	}
	st, _ := extension.DecodeStatus(got)
	if st.Enabled {
		t.Fatal("a user-disabled status must be preserved across an update")
	}
}

// TestSyncLeavesForksAlone proves a forked extension is never overwritten.
func TestSyncLeavesForksAlone(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	name := firstEntryName(t)
	forkedSpec := json.RawMessage(`{"baseURL":"https://my-fork.example.com"}`)
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion,
		Kind:       extension.Kind,
		Name:       name,
		Labels:     map[string]string{SourceLabel: SourceForked},
		Spec:       forkedSpec,
	}); err != nil {
		t.Fatalf("seed fork: %v", err)
	}
	res, err := Sync(ctx, store)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Forked < 1 {
		t.Fatalf("expected the fork to be reported, got %+v", res)
	}
	got, _ := store.Get(ctx, extension.Kind, resource.Scope{}, name)
	spec, _ := extension.DecodeSpec(got)
	if spec.BaseURL != "https://my-fork.example.com" {
		t.Fatalf("the fork was overwritten: %q", spec.BaseURL)
	}
}

// TestSyncRetiresRemoved proves a bundled extension no longer in the catalog is
// deleted, while a fork with the same fate is kept.
func TestSyncRetiresRemoved(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, Name: "gone-bundled",
		Labels: map[string]string{SourceLabel: SourceBundled},
		Spec:   json.RawMessage(`{"baseURL":"https://x"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, Name: "kept-fork",
		Labels: map[string]string{SourceLabel: SourceForked},
		Spec:   json.RawMessage(`{"baseURL":"https://y"}`),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(ctx, store)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Retired != 1 {
		t.Fatalf("expected one retirement, got %+v", res)
	}
	if _, err := store.Get(ctx, extension.Kind, resource.Scope{}, "gone-bundled"); err == nil {
		t.Fatal("the removed bundled extension should be deleted")
	}
	if _, err := store.Get(ctx, extension.Kind, resource.Scope{}, "kept-fork"); err != nil {
		t.Fatal("a fork must never be retired")
	}
}

func TestFork(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := Sync(ctx, store); err != nil {
		t.Fatal(err)
	}
	name := firstEntryName(t)
	if err := Fork(ctx, store, name); err != nil {
		t.Fatalf("fork: %v", err)
	}
	got, _ := store.Get(ctx, extension.Kind, resource.Scope{}, name)
	if got.Labels[SourceLabel] != SourceForked {
		t.Fatalf("expected forked label, got %q", got.Labels[SourceLabel])
	}
}

func TestReserved(t *testing.T) {
	name := firstEntryName(t)
	if !Reserved(name) {
		t.Fatalf("%q should be reserved", name)
	}
	if Reserved("definitely-not-an-official-extension") {
		t.Fatal("an unknown name should not be reserved")
	}
}

func firstEntryName(t *testing.T) string {
	t.Helper()
	entries, err := Entries()
	if err != nil || len(entries) == 0 {
		t.Fatalf("no entries: %v", err)
	}
	return entries[0].Name
}
