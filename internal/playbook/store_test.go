package playbook

import (
	"context"
	"encoding/json"
	"errors"
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
	// can feed the facade a record it could never have admitted.
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

// faulty builds a playbook facade over a memory store that can be made to fail.
func faulty(t *testing.T) (*Store, *faultyStore) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	fs := &faultyStore{Store: resource.NewMemory(reg)}
	return NewStore(fs), fs
}

// undecodable is a resource whose spec bytes are not a playbook object, standing in for a
// record a broken or hostile backend hands back.
func undecodable(name string) resource.Resource {
	return resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: name,
		Spec: json.RawMessage(`["not","an","object"]`),
	}
}

// TestListOrdersByName proves List returns every playbook, ordered by name regardless of
// the order they were written.
func TestListOrdersByName(t *testing.T) {
	ps, _ := stores(t)
	ctx := context.Background()
	body := Spec{Flow: json.RawMessage(`{"steps":[{"op":"return","return":{"value":"ok"}}]}`)}
	for _, n := range []string{"zulu", "alpha", "mike"} {
		putPlaybook(t, ps, n, body)
	}
	got, err := ps.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
		if len(p.Spec.Flow) == 0 {
			t.Fatalf("playbook %q lost its flow through the facade", p.Name)
		}
	}
	if strings.Join(names, ",") != "alpha,mike,zulu" {
		t.Fatalf("List is not ordered by name: %v", names)
	}
}

// TestGetMissingIsErrNotFound proves an absent playbook is reported as the package's own
// sentinel, so a caller can tell "no such playbook" apart from a backend failure.
func TestGetMissingIsErrNotFound(t *testing.T) {
	ps, _ := stores(t)
	_, err := ps.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestStoreSurfacesBackendFailures proves a backend failure is passed through as-is and is
// never mistaken for a missing playbook, on each of the three facade reads and writes.
func TestStoreSurfacesBackendFailures(t *testing.T) {
	ctx := context.Background()
	body := Spec{Flow: json.RawMessage(`{"steps":[{"op":"return","return":{"value":"ok"}}]}`)}
	down := errors.New("backend down")

	for _, tc := range []struct {
		name string
		call func(*Store) error
		set  func(*faultyStore)
	}{
		{"put", func(s *Store) error { _, err := s.Put(ctx, "p", body); return err }, func(f *faultyStore) { f.putErr = down }},
		{"get", func(s *Store) error { _, err := s.Get(ctx, "p"); return err }, func(f *faultyStore) { f.getErr = down }},
		{"list", func(s *Store) error { _, err := s.List(ctx); return err }, func(f *faultyStore) { f.listErr = down }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, fs := faulty(t)
			tc.set(fs)
			err := tc.call(ps)
			if !errors.Is(err, down) {
				t.Fatalf("expected the backend failure, got %v", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatal("a backend failure must not be reported as a missing playbook")
			}
		})
	}
}

// TestStoreRejectsUndecodableRecord proves a stored record whose spec is not a playbook
// object fails the read rather than yielding a zero-valued playbook a runner would then
// try to execute. Both the single read and the list read must refuse it.
func TestStoreRejectsUndecodableRecord(t *testing.T) {
	ctx := context.Background()

	ps, fs := faulty(t)
	rec := undecodable("p")
	fs.getRec = &rec
	if _, err := ps.Get(ctx, "p"); err == nil {
		t.Fatal("Get must refuse a record that is not a playbook spec")
	}

	ps2, fs2 := faulty(t)
	fs2.listRec = []resource.Resource{undecodable("p")}
	if _, err := ps2.List(ctx); err == nil {
		t.Fatal("List must refuse a record that is not a playbook spec")
	}
}

// TestPutRefusesUnencodableSpec proves a spec carrying malformed raw JSON (a flow that is
// not valid JSON) is refused at encode time rather than being written to the store, where
// it would fail only later at run time.
func TestPutRefusesUnencodableSpec(t *testing.T) {
	ps, _ := stores(t)
	_, err := ps.Put(context.Background(), "p", Spec{Flow: json.RawMessage(`{"steps":`)})
	if err == nil {
		t.Fatal("a spec whose flow is not valid JSON must be refused")
	}
	if !strings.Contains(err.Error(), "playbook:") {
		t.Fatalf("the error should name the package: %v", err)
	}
}

// TestDecodeSpecRejectsWrongShape proves the typed decode refuses spec bytes that are not a
// playbook object, so a mistyped resource never becomes a silently empty playbook.
func TestDecodeSpecRejectsWrongShape(t *testing.T) {
	if _, err := DecodeSpec(undecodable("p")); err == nil {
		t.Fatal("decoding a non-object spec must fail")
	}
	got, err := DecodeSpec(resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "p",
		Spec: json.RawMessage(`{"description":"d","flow":{"steps":[]}}`),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Description != "d" || len(got.Flow) == 0 {
		t.Fatalf("decoded spec lost fields: %+v", got)
	}
}
