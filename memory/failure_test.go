package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// errBackend is the failure a broken or unreachable resource backend reports.
var errBackend = errors.New("backend unreachable")

// faultyStore wraps a resource store and fails the selected operations, modelling a
// backend that is down. Anything not overridden delegates to the embedded store.
type faultyStore struct {
	resource.Store
	putErr     error
	getErr     error
	listErr    error
	listAllErr error
}

func (f faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if f.getErr != nil {
		return resource.Resource{}, f.getErr
	}
	return f.Store.Get(ctx, kind, scope, name)
}

func (f faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if f.putErr != nil {
		return resource.Resource{}, f.putErr
	}
	return f.Store.Put(ctx, r)
}

func (f faultyStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.List(ctx, kind, scope, sel)
}

func (f faultyStore) ListAll(ctx context.Context, kind string, sel resource.Selector) ([]resource.Resource, error) {
	if f.listAllErr != nil {
		return nil, f.listAllErr
	}
	return f.Store.ListAll(ctx, kind, sel)
}

// corruptStore serves one record whose spec is not a memory object, modelling a record
// written by an incompatible version.
type corruptStore struct{ resource.Store }

func badResources() []resource.Resource {
	return []resource.Resource{{
		APIVersion: memory.GroupVersion,
		Kind:       memory.Kind,
		Name:       "mem-1",
		Spec:       json.RawMessage(`"not-an-object"`),
	}}
}

func (corruptStore) List(context.Context, string, resource.Scope, resource.Selector) ([]resource.Resource, error) {
	return badResources(), nil
}

func (corruptStore) ListAll(context.Context, string, resource.Selector) ([]resource.Resource, error) {
	return badResources(), nil
}

// backing builds an in-memory resource store with the Memory kind registered, so a
// wrapper can fail one operation and delegate the rest.
func backing(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := memory.RegisterKind(reg); err != nil {
		t.Fatalf("register memory kind: %v", err)
	}
	return resource.NewMemory(reg)
}

// TestWriteTranslatesConflict proves a version conflict from the foundation reaches the
// caller as the state boundary's sentinel, so a MemoryStore caller can retry on it
// without knowing which backend is underneath.
func TestWriteTranslatesConflict(t *testing.T) {
	s := memory.NewStore(faultyStore{
		Store:  backing(t),
		putErr: fmt.Errorf("stale write: %w", resource.ErrConflict),
	})
	_, err := s.Write(context.Background(), state.MemoryItem{Content: "hi"})
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("Write error = %v, want state.ErrConflict", err)
	}
}

// TestWriteBackendErrorPropagates proves an outage is passed through untranslated, so
// it is never mistaken for a conflict or a missing item.
func TestWriteBackendErrorPropagates(t *testing.T) {
	s := memory.NewStore(faultyStore{Store: backing(t), putErr: errBackend})
	_, err := s.Write(context.Background(), state.MemoryItem{Content: "hi"})
	if !errors.Is(err, errBackend) {
		t.Fatalf("Write error = %v, want the backend error", err)
	}
	if errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) {
		t.Fatalf("an outage must not be translated to a state sentinel: %v", err)
	}
}

// TestDeleteMissingIsNotFound proves deleting an unknown id reports the state
// boundary's not-found sentinel.
func TestDeleteMissingIsNotFound(t *testing.T) {
	s := memory.NewStore(backing(t))
	if err := s.Delete(context.Background(), "no-such-id"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete error = %v, want state.ErrNotFound", err)
	}
}

// TestRecallBackendErrorsPropagate proves a read outage is reported on both recall
// paths: the scope-spanning one and the scoped one.
func TestRecallBackendErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	spanning := memory.NewStore(faultyStore{Store: backing(t), listAllErr: errBackend})
	if _, err := spanning.Recall(ctx, state.RecallQuery{}); !errors.Is(err, errBackend) {
		t.Fatalf("scope-spanning recall error = %v", err)
	}
	scoped := memory.NewStore(faultyStore{Store: backing(t), listErr: errBackend})
	if _, err := scoped.Recall(ctx, state.RecallQuery{Scope: state.Scope{Instance: "i1"}}); !errors.Is(err, errBackend) {
		t.Fatalf("scoped recall error = %v", err)
	}
	// A widened recall reads one scope per ancestor, so the outage has to be
	// reported from inside that walk rather than swallowed after the first level.
	widened := memory.NewStore(faultyStore{Store: backing(t), listErr: errBackend})
	q := state.RecallQuery{Scope: state.Scope{Instance: "i1", Project: "p", Workspace: "w"}, IncludeAncestors: true}
	if _, err := widened.Recall(ctx, q); !errors.Is(err, errBackend) {
		t.Fatalf("widened recall error = %v", err)
	}
}

// TestRecallSurfacesCorruptSpec proves an item that cannot be decoded fails the recall
// instead of being returned as an empty memory.
func TestRecallSurfacesCorruptSpec(t *testing.T) {
	s := memory.NewStore(corruptStore{backing(t)})
	ctx := context.Background()
	if _, err := s.Recall(ctx, state.RecallQuery{}); err == nil {
		t.Fatal("a corrupt record must fail the scope-spanning recall")
	}
	if _, err := s.Recall(ctx, state.RecallQuery{Scope: state.Scope{Instance: "i1"}}); err == nil {
		t.Fatal("a corrupt record must fail the scoped recall")
	}
}

// promotedItem writes an item through s and returns its id, so a promotion test has
// something live to decide about.
func promotedItem(t *testing.T, s *memory.Store) string {
	t.Helper()
	it, err := s.Write(context.Background(), state.MemoryItem{Kind: "fact", Content: "decide about me"})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}
	return it.ID
}

// TestPromoteReportsBackendFailures proves an outage on either half of the decision
// write is reported rather than swallowed. The read of what is already on file
// matters most: a lookup error read as "nobody has decided yet" would overwrite a
// reviewer's answer the store merely could not see.
func TestPromoteReportsBackendFailures(t *testing.T) {
	ctx := context.Background()
	back := backing(t)
	seeded := memory.NewStore(back)
	d := state.PromotionDecision{MemoryID: promotedItem(t, seeded), Promoted: true, By: "user:operator"}

	onRead := memory.NewStore(faultyStore{Store: back, getErr: errBackend})
	if _, err := onRead.Promote(ctx, d); !errors.Is(err, errBackend) {
		t.Fatalf("Promote error on the prior-decision read = %v, want the backend error", err)
	}
	onWrite := memory.NewStore(faultyStore{Store: back, putErr: errBackend})
	if _, err := onWrite.Promote(ctx, d); !errors.Is(err, errBackend) {
		t.Fatalf("Promote error on the write = %v, want the backend error", err)
	}
	// An item that is not there is the state boundary's not-found, whatever the
	// backend called it.
	if _, err := seeded.Promote(ctx, state.PromotionDecision{MemoryID: "no-such-id", Promoted: true, By: "user:operator"}); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Promote of an unknown item = %v, want state.ErrNotFound", err)
	}
}

// TestPromotionsReportBackendFailures proves the read side surfaces an outage and a
// record it cannot decode, rather than reporting an empty or zero-valued decision -
// either of which the digest would read as "nobody promoted this".
func TestPromotionsReportBackendFailures(t *testing.T) {
	ctx := context.Background()
	down := memory.NewStore(faultyStore{Store: backing(t), listAllErr: errBackend})
	if _, err := down.Promotions(ctx, nil); !errors.Is(err, errBackend) {
		t.Fatalf("Promotions error = %v, want the backend error", err)
	}
	corrupt := memory.NewStore(corruptPromotions{backing(t)})
	if _, err := corrupt.Promotions(ctx, nil); err == nil {
		t.Fatal("a promotion record that cannot be decoded must fail the read")
	}
}

// corruptPromotions serves one decision record whose spec is not an object,
// modelling a record written by an incompatible version.
type corruptPromotions struct{ resource.Store }

func (corruptPromotions) ListAll(context.Context, string, resource.Selector) ([]resource.Resource, error) {
	return []resource.Resource{{
		APIVersion: memory.GroupVersion,
		Kind:       memory.PromotionKind,
		Name:       "mempromo-1",
		Spec:       json.RawMessage(`"not-an-object"`),
	}}, nil
}
