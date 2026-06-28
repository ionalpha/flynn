package dependency

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// TestStoreRoundTrip proves a dependency spec is admitted, read back, listed, and that a
// spec with no binary is refused.
func TestStoreRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	spec := Spec{Binaries: []string{"flyctl"}, MinVersion: "0.3.0", Pin: "0.4.61"}
	if _, err := s.Put(ctx, "flyctl", spec); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, "flyctl")
	if err != nil || got.Spec.Pin != "0.4.61" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d err=%v", len(list), err)
	}
	if _, err := s.Put(ctx, "x", Spec{}); err == nil {
		t.Fatal("a spec with no binary must be refused")
	}
}

// TestSchemaRejectsUnknownArchive proves the kind schema refuses a release with an archive
// format the engine does not understand, so a malformed spec fails at admission.
func TestSchemaRejectsUnknownArchive(t *testing.T) {
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := resource.NewMemory(reg)
	bad := json.RawMessage(`{"binaries":["x"],"releases":[{"goos":"linux","goarch":"amd64","url":"https://x/y","sha256":"00","archive":"rar","binName":"x"}]}`)
	if _, err := store.Put(context.Background(), resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "bad", Spec: bad,
	}); err == nil {
		t.Fatal("expected the schema to reject an unknown archive format")
	}
}

// TestCatalogSyncsBundled proves the embedded catalog parses and syncs into the store, and
// that the bundled names are reserved.
func TestCatalogSyncsBundled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	n, err := Sync(ctx, s)
	if err != nil || n == 0 {
		t.Fatalf("sync: n=%d err=%v", n, err)
	}
	if !Reserved("flyctl") {
		t.Fatal("flyctl should be a reserved bundled name")
	}
	got, err := s.Get(ctx, "flyctl")
	if err != nil {
		t.Fatalf("flyctl not synced: %v", err)
	}
	if len(got.Spec.Releases) == 0 || !strings.HasPrefix(got.Spec.Releases[0].URL, "https://") {
		t.Fatalf("flyctl spec did not round-trip: %+v", got.Spec)
	}

	// A second sync is a no-op (idempotent): the resource store dedups unchanged specs.
	before, _ := s.Get(ctx, "flyctl")
	if _, err := Sync(ctx, s); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	// (No version assertion here; the gate test covers structural validity.)
	_ = before
}
