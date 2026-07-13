package playbook

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// TestCatalogEntriesAreSortedAndComplete proves the embedded catalog parses, is ordered by
// name, and carries both the decoded spec and the raw bytes the kind schema admitted, which
// is what Sync and the gate each rely on.
func TestCatalogEntriesAreSortedAndComplete(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the playbook catalog is empty")
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
		if len(e.Raw) == 0 || len(e.Spec.Flow) == 0 {
			t.Fatalf("catalog entry %q is missing its raw bytes or its flow", e.Name)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("the catalog is not ordered by name: %v", names)
	}
}

// TestReservedNames proves a bundled playbook's name is reserved and an unbundled one is
// not, which is the check that stops a runtime-authored playbook from impersonating an
// official one.
func TestReservedNames(t *testing.T) {
	if !Reserved("fly-app") {
		t.Fatal("fly-app is bundled and must be reserved")
	}
	if Reserved("not-a-bundled-playbook") {
		t.Fatal("an unbundled name must not be reserved")
	}
	if Reserved("") {
		t.Fatal("the empty name must not be reserved")
	}
}

// TestSyncWritesEveryBundledPlaybook proves Sync writes the whole catalog into the store,
// reports how many it wrote, and is idempotent: a second sync writes the same content and
// leaves the same set of playbooks behind.
func TestSyncWritesEveryBundledPlaybook(t *testing.T) {
	ps, _ := stores(t)
	ctx := context.Background()

	n, err := Sync(ctx, ps)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if n != len(entries) {
		t.Fatalf("sync wrote %d playbooks, the catalog holds %d", n, len(entries))
	}
	first, err := ps.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != len(entries) {
		t.Fatalf("stored %d playbooks, expected %d", len(first), len(entries))
	}

	if _, err := Sync(ctx, ps); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	second, err := ps.List(ctx)
	if err != nil {
		t.Fatalf("list after re-sync: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("re-syncing changed the playbook set: %d then %d", len(first), len(second))
	}
	for i := range second {
		if second[i].Name != first[i].Name || string(second[i].Spec.Flow) != string(first[i].Spec.Flow) {
			t.Fatalf("re-syncing changed %q", second[i].Name)
		}
	}
}

// TestSyncFailsWhenTheStoreRejectsAPlaybook proves a write failure stops the sync with an
// error naming the playbook that could not be written, rather than reporting a count that
// pretends the catalog is in the store.
func TestSyncFailsWhenTheStoreRejectsAPlaybook(t *testing.T) {
	ps, fs := faulty(t)
	down := errors.New("backend down")
	fs.putErr = down

	n, err := Sync(context.Background(), ps)
	if err == nil {
		t.Fatal("a store that refuses the write must fail the sync")
	}
	if !errors.Is(err, down) {
		t.Fatalf("the sync error must wrap the store failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("a failed sync must report no playbooks written, reported %d", n)
	}
}
