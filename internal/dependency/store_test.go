package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// faultyStore wraps a resource.Store and fails or rewrites the calls a test names, so the
// typed facade's failure paths (a backend that is down, a stored record that will not
// decode) are exercised without a real backend. The embedded interface supplies every
// method the facade does not use.
type faultyStore struct {
	resource.Store
	putErr  error
	getErr  error
	listErr error
	// getRec, when set, is returned by Get instead of the inner store's record, so a test
	// can feed the engine a spec the kind schema would never have admitted.
	getRec  *resource.Resource
	listRec []resource.Resource
}

func (s *faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if s.putErr != nil {
		return resource.Resource{}, s.putErr
	}
	return s.Store.Put(ctx, r)
}

func (s *faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if s.getErr != nil {
		return resource.Resource{}, s.getErr
	}
	if s.getRec != nil {
		return *s.getRec, nil
	}
	return s.Store.Get(ctx, kind, scope, name)
}

func (s *faultyStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listRec != nil {
		return s.listRec, nil
	}
	return s.Store.List(ctx, kind, scope, sel)
}

// faulty builds a dependency facade over a memory store that can be made to fail.
func faulty(t *testing.T) (*Store, *faultyStore) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	fs := &faultyStore{Store: resource.NewMemory(reg)}
	return NewStore(fs), fs
}

// undecodable is a resource whose spec bytes are not a dependency object, standing in for a
// record a broken or hostile backend hands back.
func undecodable(name string) resource.Resource {
	return resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: name,
		Spec: json.RawMessage(`["not","an","object"]`),
	}
}

// TestListOrdersByName proves List returns every dependency spec, ordered by name whatever
// order they were written in.
func TestListOrdersByName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, n := range []string{"zulu", "alpha", "mike"} {
		if _, err := s.Put(ctx, n, Spec{Binaries: []string{n}}); err != nil {
			t.Fatalf("put %s: %v", n, err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Name
		if len(d.Spec.Binaries) == 0 {
			t.Fatalf("dependency %q lost its binaries through the facade", d.Name)
		}
	}
	if !sort.StringsAreSorted(names) || strings.Join(names, ",") != "alpha,mike,zulu" {
		t.Fatalf("List is not ordered by name: %v", names)
	}
}

// TestGetMissingIsErrNotFound proves an absent spec is reported as the package's own
// sentinel, so a caller can tell "no such dependency" apart from a backend failure.
func TestGetMissingIsErrNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestStoreSurfacesBackendFailures proves a backend failure is passed through as-is and is
// never mistaken for a missing dependency, on each of the three facade reads and writes.
func TestStoreSurfacesBackendFailures(t *testing.T) {
	ctx := context.Background()
	spec := Spec{Binaries: []string{"flyctl"}}
	down := errors.New("backend down")

	for _, tc := range []struct {
		name string
		call func(*Store) error
		set  func(*faultyStore)
	}{
		{"put", func(s *Store) error { _, err := s.Put(ctx, "flyctl", spec); return err }, func(f *faultyStore) { f.putErr = down }},
		{"get", func(s *Store) error { _, err := s.Get(ctx, "flyctl"); return err }, func(f *faultyStore) { f.getErr = down }},
		{"list", func(s *Store) error { _, err := s.List(ctx); return err }, func(f *faultyStore) { f.listErr = down }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fs := faulty(t)
			tc.set(fs)
			err := tc.call(s)
			if !errors.Is(err, down) {
				t.Fatalf("expected the backend failure, got %v", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatal("a backend failure must not be reported as a missing dependency")
			}
		})
	}
}

// TestStoreRejectsUndecodableRecord proves a stored record whose spec is not a dependency
// object fails the read rather than yielding a zero-valued spec the engine would then try to
// satisfy (a spec with no binaries and no releases). Both the single read and the list read
// must refuse it.
func TestStoreRejectsUndecodableRecord(t *testing.T) {
	ctx := context.Background()

	s, fs := faulty(t)
	rec := undecodable("flyctl")
	fs.getRec = &rec
	if _, err := s.Get(ctx, "flyctl"); err == nil {
		t.Fatal("Get must refuse a record that is not a dependency spec")
	}

	s2, fs2 := faulty(t)
	fs2.listRec = []resource.Resource{undecodable("flyctl")}
	if _, err := s2.List(ctx); err == nil {
		t.Fatal("List must refuse a record that is not a dependency spec")
	}
}

// TestDecodeSpecRejectsWrongShape proves the typed decode refuses spec bytes that are not a
// dependency object, so a mistyped resource never becomes a silently empty spec.
func TestDecodeSpecRejectsWrongShape(t *testing.T) {
	if _, err := DecodeSpec(undecodable("flyctl")); err == nil {
		t.Fatal("decoding a non-object spec must fail")
	}
	got, err := DecodeSpec(resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "flyctl",
		Spec: json.RawMessage(`{"binaries":["flyctl"],"pin":"0.4.61"}`),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Pin != "0.4.61" || len(got.Binaries) != 1 {
		t.Fatalf("decoded spec lost fields: %+v", got)
	}
}

// TestReservedNames proves a bundled dependency's name is reserved and an unbundled one is
// not, which is the check that stops a runtime-authored spec from impersonating an official
// program and having Flynn install something else under its name.
func TestReservedNames(t *testing.T) {
	if !Reserved("flyctl") {
		t.Fatal("flyctl is bundled and must be reserved")
	}
	if Reserved("not-a-bundled-dependency") {
		t.Fatal("an unbundled name must not be reserved")
	}
	if Reserved("") {
		t.Fatal("the empty name must not be reserved")
	}
}

// TestCatalogEntriesAreSortedAndComplete proves the embedded catalog parses, is ordered by
// name, and carries both the decoded spec and the raw bytes the kind schema admitted, which
// is what Sync and the gate each rely on.
func TestCatalogEntriesAreSortedAndComplete(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the dependency catalog is empty")
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
		if len(e.Raw) == 0 || len(e.Spec.Binaries) == 0 {
			t.Fatalf("catalog entry %q is missing its raw bytes or its binaries", e.Name)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("the catalog is not ordered by name: %v", names)
	}
}

// TestSyncFailsWhenTheStoreRejectsASpec proves a write failure stops the sync with an error
// wrapping the store's, rather than reporting a count that pretends the catalog is in the
// store.
func TestSyncFailsWhenTheStoreRejectsASpec(t *testing.T) {
	s, fs := faulty(t)
	down := errors.New("backend down")
	fs.putErr = down

	n, err := Sync(context.Background(), s)
	if err == nil {
		t.Fatal("a store that refuses the write must fail the sync")
	}
	if !errors.Is(err, down) {
		t.Fatalf("the sync error must wrap the store failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("a failed sync must report no specs written, reported %d", n)
	}
}
