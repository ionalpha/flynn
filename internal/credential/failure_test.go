package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// errBackend is the failure a broken or unreachable resource backend reports.
var errBackend = errors.New("backend unreachable")

// faultyStore wraps a resource store and fails the selected operations, modelling a
// backend that is down. Anything not overridden delegates to the embedded store, so a
// test can fail exactly one operation and keep the rest working.
type faultyStore struct {
	resource.Store
	putErr  error
	getErr  error
	listErr error
	delErr  error
}

func (f faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if f.putErr != nil {
		return resource.Resource{}, f.putErr
	}
	return f.Store.Put(ctx, r)
}

func (f faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if f.getErr != nil {
		return resource.Resource{}, f.getErr
	}
	return f.Store.Get(ctx, kind, scope, name)
}

func (f faultyStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.List(ctx, kind, scope, sel)
}

func (f faultyStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	return f.Store.Delete(ctx, kind, scope, name)
}

// corruptStore returns one resource whose spec is not a credential object, modelling a
// record written by an incompatible version. Every read path must surface the decode
// error rather than a half-built credential.
type corruptStore struct{ resource.Store }

func badResource() resource.Resource {
	return resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       "cf/prod",
		Spec:       json.RawMessage(`"not-an-object"`),
	}
}

func (corruptStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return badResource(), nil
}

func (corruptStore) List(context.Context, string, resource.Scope, resource.Selector) ([]resource.Resource, error) {
	return []resource.Resource{badResource()}, nil
}

// memStore is the working in-memory store the faulty wrappers sit on.
func memStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

// TestRefUsesExplicitVaultRef proves an explicit vault reference wins over the
// conventional "<integration>/<name>", so a credential whose secret lives under a
// legacy key still resolves to the right vault entry.
func TestRefUsesExplicitVaultRef(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, Spec{Integration: "cf", Name: "prod", VaultRef: "legacy/cf-token"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, "cf", "prod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Ref() != "legacy/cf-token" {
		t.Fatalf("Ref() = %q, want the explicit vault ref", got.Ref())
	}
	if VaultRef("cf", "prod") != "cf/prod" {
		t.Fatalf("VaultRef built %q", VaultRef("cf", "prod"))
	}
}

// TestAllSpansIntegrations proves All returns every credential ordered by integration
// then name, the ordering a cross-integration listing depends on.
func TestAllSpansIntegrations(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "vercel", Name: "b"})
	mustPut(t, s, Spec{Integration: "cf", Name: "b"})
	mustPut(t, s, Spec{Integration: "cf", Name: "a"})
	mustPut(t, s, Spec{Integration: "vercel", Name: "a"})

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	var got []string
	for _, c := range all {
		got = append(got, c.Ref())
	}
	want := []string{"cf/a", "cf/b", "vercel/a", "vercel/b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("All order = %v, want %v", got, want)
	}
}

// TestDecodeStatus proves a stored status round-trips and a malformed one is reported
// as an error rather than read as an empty status.
func TestDecodeStatus(t *testing.T) {
	ok := resource.Resource{Status: json.RawMessage(`{"lastUsed":"2026-01-02T03:04:05Z"}`)}
	st, err := DecodeStatus(ok)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.LastUsed != "2026-01-02T03:04:05Z" {
		t.Fatalf("status: %+v", st)
	}
	if _, err := DecodeStatus(resource.Resource{Status: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("a malformed status must be an error")
	}
}

// TestDecodeSpecMalformed proves a resource whose spec is not a credential object is
// rejected by the decoder.
func TestDecodeSpecMalformed(t *testing.T) {
	if _, err := DecodeSpec(badResource()); err == nil {
		t.Fatal("a malformed spec must be an error")
	}
}

// TestReadPathsSurfaceCorruptSpec proves a record that cannot be decoded fails the read
// instead of yielding a zero credential: Get, List, and All all report it.
func TestReadPathsSurfaceCorruptSpec(t *testing.T) {
	s := NewStore(corruptStore{memStore(t)})
	ctx := context.Background()
	if _, err := s.Get(ctx, "cf", "prod"); err == nil {
		t.Fatal("Get must surface the decode error")
	}
	if _, err := s.List(ctx, "cf"); err == nil {
		t.Fatal("List must surface the decode error")
	}
	if _, err := s.All(ctx); err == nil {
		t.Fatal("All must surface the decode error")
	}
	if _, err := s.Default(ctx, "cf"); err == nil {
		t.Fatal("Default must surface the decode error")
	}
}

// TestBackendErrorsPropagate proves a store failure is passed through untranslated on
// every operation, so a caller retries rather than mistaking an outage for a missing
// credential.
func TestBackendErrorsPropagate(t *testing.T) {
	ctx := context.Background()

	putBroken := NewStore(faultyStore{Store: memStore(t), putErr: errBackend})
	if _, err := putBroken.Put(ctx, Spec{Integration: "cf", Name: "prod"}); !errors.Is(err, errBackend) {
		t.Fatalf("Put error = %v, want the backend error", err)
	}
	// A default Put writes first, so the same failure is reported before any clearing.
	if _, err := putBroken.Put(ctx, Spec{Integration: "cf", Name: "prod", IsDefault: true}); !errors.Is(err, errBackend) {
		t.Fatalf("default Put error = %v, want the backend error", err)
	}

	listBroken := NewStore(faultyStore{Store: memStore(t), listErr: errBackend})
	if _, err := listBroken.List(ctx, "cf"); !errors.Is(err, errBackend) {
		t.Fatalf("List error = %v", err)
	}
	if _, err := listBroken.All(ctx); !errors.Is(err, errBackend) {
		t.Fatalf("All error = %v", err)
	}
	if _, err := listBroken.Default(ctx, "cf"); !errors.Is(err, errBackend) {
		t.Fatalf("Default error = %v", err)
	}
	if _, err := listBroken.Resolve(ctx, "cf"); !errors.Is(err, errBackend) {
		t.Fatalf("Resolve of a bare integration error = %v", err)
	}

	getBroken := NewStore(faultyStore{Store: memStore(t), getErr: errBackend})
	if err := getBroken.SetDefault(ctx, "cf", "prod"); !errors.Is(err, errBackend) {
		t.Fatalf("SetDefault error = %v", err)
	}

	delBroken := NewStore(faultyStore{Store: memStore(t), delErr: errBackend})
	if err := delBroken.Delete(ctx, "cf", "prod"); !errors.Is(err, errBackend) {
		t.Fatalf("Delete error = %v", err)
	}
}

// TestClearDefaultsFailurePropagates proves that when displacing an existing default
// fails partway, the error reaches the caller: a silent failure would leave the
// integration with two defaults.
func TestClearDefaultsFailurePropagates(t *testing.T) {
	ctx := context.Background()
	backing := memStore(t)
	s := NewStore(backing)
	mustPut(t, s, Spec{Integration: "cf", Name: "a", IsDefault: true})

	// The list that clearDefaults runs to find the old default now fails.
	broken := NewStore(faultyStore{Store: backing, listErr: errBackend})
	if _, err := broken.Put(ctx, Spec{Integration: "cf", Name: "b", IsDefault: true}); !errors.Is(err, errBackend) {
		t.Fatalf("a failed default-clear must propagate, got %v", err)
	}
}

// putFailAfter fails every Put past the first n, so a multi-write operation can be
// broken partway through.
type putFailAfter struct {
	resource.Store
	n    int
	puts int
}

func (p *putFailAfter) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	p.puts++
	if p.puts > p.n {
		return resource.Resource{}, errBackend
	}
	return p.Store.Put(ctx, r)
}

// TestClearDefaultsWriteFailurePropagates proves that when the write that unsets the
// previous default fails, the error reaches the caller rather than leaving the
// integration silently holding two defaults.
func TestClearDefaultsWriteFailurePropagates(t *testing.T) {
	ctx := context.Background()
	backing := memStore(t)
	mustPut(t, NewStore(backing), Spec{Integration: "cf", Name: "a", IsDefault: true})

	// The new default is written (Put 1), then the clear of "a" fails (Put 2).
	broken := NewStore(&putFailAfter{Store: backing, n: 1})
	if _, err := broken.Put(ctx, Spec{Integration: "cf", Name: "b", IsDefault: true}); !errors.Is(err, errBackend) {
		t.Fatalf("a failed default-clear write must propagate, got %v", err)
	}
}

// TestTranslateErrMapsResourceSentinels proves the resource layer's sentinels are
// mapped onto this package's, and any other error is passed through unchanged.
func TestTranslateErrMapsResourceSentinels(t *testing.T) {
	if err := translateErr(nil); err != nil {
		t.Fatalf("nil must translate to nil, got %v", err)
	}
	notFound := translateErr(fmt.Errorf("wrapped: %w", resource.ErrNotFound))
	if !errors.Is(notFound, ErrNotFound) {
		t.Fatalf("resource.ErrNotFound must map to ErrNotFound, got %v", notFound)
	}
	invalid := translateErr(fmt.Errorf("wrapped: %w", resource.ErrInvalid))
	if !errors.Is(invalid, ErrInvalid) || !errors.Is(invalid, resource.ErrInvalid) {
		t.Fatalf("resource.ErrInvalid must map to ErrInvalid and keep the cause, got %v", invalid)
	}
	if err := translateErr(errBackend); !errors.Is(err, errBackend) {
		t.Fatalf("an unknown error must pass through, got %v", err)
	}
}

// TestPutRejectsUnknownRoleBeforeWriting proves an invalid role never reaches the
// store: a credential with an unrecognized privilege level is refused, not stored with
// a role no gate understands.
func TestPutRejectsUnknownRoleBeforeWriting(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, Spec{Integration: "cf", Name: "x", Role: "root", IsDefault: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := s.Get(ctx, "cf", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the rejected credential must not have been written, got %v", err)
	}
}
