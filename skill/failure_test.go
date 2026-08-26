package skill_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/state"
)

// errBackend is the failure a broken or unreachable resource backend reports.
var errBackend = errors.New("backend unreachable")

// faultyStore wraps a resource store and fails the selected operations, modelling a
// backend that is down. Anything not overridden delegates to the embedded store.
type faultyStore struct {
	resource.Store
	putErr     error
	getByIDErr error
	listErr    error
	listAllErr error
	delErr     error
}

func (f faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if f.putErr != nil {
		return resource.Resource{}, f.putErr
	}
	return f.Store.Put(ctx, r)
}

func (f faultyStore) GetByID(ctx context.Context, id string) (resource.Resource, error) {
	if f.getByIDErr != nil {
		return resource.Resource{}, f.getByIDErr
	}
	return f.Store.GetByID(ctx, id)
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

func (f faultyStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	return f.Store.Delete(ctx, kind, scope, name)
}

// corruptStore serves one record whose spec is not a skill object, modelling a record
// written by an incompatible version.
type corruptStore struct{ resource.Store }

func badResources() []resource.Resource {
	return []resource.Resource{{
		APIVersion: skill.GroupVersion,
		Kind:       skill.Kind,
		Name:       "deploy",
		Spec:       json.RawMessage(`"not-an-object"`),
	}}
}

func (corruptStore) List(context.Context, string, resource.Scope, resource.Selector) ([]resource.Resource, error) {
	return badResources(), nil
}

func (corruptStore) ListAll(context.Context, string, resource.Selector) ([]resource.Resource, error) {
	return badResources(), nil
}

func (corruptStore) GetByID(context.Context, string) (resource.Resource, error) {
	return resource.Resource{}, resource.ErrNotFound
}

// backing builds an in-memory resource store with the Skill kind registered, so a
// wrapper can fail one operation and delegate the rest.
func backing(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := skill.RegisterKind(reg); err != nil {
		t.Fatalf("register skill kind: %v", err)
	}
	return resource.NewMemory(reg)
}

// TestUpsertTranslatesConflict proves a version conflict from the foundation reaches
// the caller as the state boundary's sentinel, the signal a caller retries on.
func TestUpsertTranslatesConflict(t *testing.T) {
	s := skill.NewStore(faultyStore{
		Store:  backing(t),
		putErr: fmt.Errorf("stale write: %w", resource.ErrConflict),
	})
	_, err := s.Upsert(context.Background(), state.Skill{Slug: "deploy", Body: "b"})
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("Upsert error = %v, want state.ErrConflict", err)
	}
}

// TestUpsertBackendErrorPropagates proves an outage passes through untranslated, so it
// is never mistaken for a conflict.
func TestUpsertBackendErrorPropagates(t *testing.T) {
	s := skill.NewStore(faultyStore{Store: backing(t), putErr: errBackend})
	_, err := s.Upsert(context.Background(), state.Skill{Slug: "deploy"})
	if !errors.Is(err, errBackend) {
		t.Fatalf("Upsert error = %v, want the backend error", err)
	}
	if errors.Is(err, state.ErrConflict) {
		t.Fatalf("an outage must not be translated to a conflict: %v", err)
	}
}

// TestReadBackendErrorsPropagate proves a read outage is reported on each read path
// rather than being read as "no such skill".
func TestReadBackendErrorsPropagate(t *testing.T) {
	ctx := context.Background()

	listBroken := skill.NewStore(faultyStore{Store: backing(t), listErr: errBackend})
	if _, err := listBroken.List(ctx, state.Scope{Instance: "i1"}); !errors.Is(err, errBackend) {
		t.Fatalf("List error = %v", err)
	}

	scanBroken := skill.NewStore(faultyStore{Store: backing(t), listAllErr: errBackend})
	if _, err := scanBroken.Search(ctx, "", 0); !errors.Is(err, errBackend) {
		t.Fatalf("Search error = %v", err)
	}
	// A slug resolve falls back to the scope-spanning scan once the id lookup misses.
	if _, err := scanBroken.Get(ctx, "deploy"); !errors.Is(err, errBackend) {
		t.Fatalf("Get by slug error = %v", err)
	}

	idBroken := skill.NewStore(faultyStore{Store: backing(t), getByIDErr: errBackend})
	_, err := idBroken.Get(ctx, "some-id")
	if !errors.Is(err, errBackend) || errors.Is(err, state.ErrNotFound) {
		t.Fatalf("a failed id lookup must report the outage, not a missing skill: %v", err)
	}
}

// TestDeleteBackendErrorPropagates proves a tombstone that the backend refuses is
// reported, so the caller does not believe the skill is gone.
func TestDeleteBackendErrorPropagates(t *testing.T) {
	ctx := context.Background()
	back := backing(t)
	if _, err := skill.NewStore(back).Upsert(ctx, state.Skill{Slug: "deploy", Body: "b"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	broken := skill.NewStore(faultyStore{Store: back, delErr: errBackend})
	if err := broken.Delete(ctx, "deploy"); !errors.Is(err, errBackend) {
		t.Fatalf("Delete error = %v", err)
	}
}

// TestDeleteMissingIsNotFound proves deleting an unknown handle reports the state
// boundary's not-found sentinel.
func TestDeleteMissingIsNotFound(t *testing.T) {
	s := skill.NewStore(backing(t))
	if err := s.Delete(context.Background(), "ghost"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Delete error = %v, want state.ErrNotFound", err)
	}
}

// TestReadPathsSurfaceCorruptSpec proves a record that cannot be decoded fails the read
// rather than being returned as an empty skill.
func TestReadPathsSurfaceCorruptSpec(t *testing.T) {
	s := skill.NewStore(corruptStore{backing(t)})
	ctx := context.Background()
	if _, err := s.List(ctx, state.Scope{}); err == nil {
		t.Fatal("List must surface the decode error")
	}
	if _, err := s.Search(ctx, "", 0); err == nil {
		t.Fatal("Search must surface the decode error")
	}
	if _, err := s.Get(ctx, "deploy"); err == nil {
		t.Fatal("Get by slug must surface the decode error")
	}
}

// TestSearchOrdersSameSlugAcrossScopes proves the search order is total: the same slug
// in two scopes is broken by id, so the result is deterministic rather than dependent
// on store iteration order.
func TestSearchOrdersSameSlugAcrossScopes(t *testing.T) {
	ctx := context.Background()
	s := skill.NewStore(backing(t))
	a, err := s.Upsert(ctx, state.Skill{Slug: "deploy", Body: "one", Scope: state.Scope{Instance: "i1"}})
	if err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	b, err := s.Upsert(ctx, state.Skill{Slug: "deploy", Body: "two", Scope: state.Scope{Instance: "i2"}})
	if err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	got, err := s.Search(ctx, "", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("search returned %d skills, want both", len(got))
	}
	first, second := a.ID, b.ID
	if second < first {
		first, second = second, first
	}
	if got[0].ID != first || got[1].ID != second {
		t.Fatalf("same-slug skills must be ordered by id: got %s,%s want %s,%s", got[0].ID, got[1].ID, first, second)
	}
}

// TestSearchCapsToTheBestMatches proves the limit selects on how well a skill
// answers the query, not on how its slug sorts. Recall asks for a bounded number of
// candidates per term, so a cut that ran alphabetically would hide the skill the
// query is about behind any number of skills that merely mention the word, and
// nothing would report it.
func TestSearchCapsToTheBestMatches(t *testing.T) {
	ctx := context.Background()
	s := skill.NewStore(backing(t))
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.Upsert(ctx, state.Skill{Slug: slug, Body: "a deploy is mentioned here"}); err != nil {
			t.Fatalf("upsert %s: %v", slug, err)
		}
	}
	if _, err := s.Upsert(ctx, state.Skill{
		Slug:        "zeta",
		Description: "How to deploy the service.",
	}); err != nil {
		t.Fatalf("upsert zeta: %v", err)
	}

	got, err := s.Search(ctx, "deploy", 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].Slug != "zeta" {
		t.Fatalf("search with limit 1 = %+v, want zeta: the description is where a skill states its subject", got)
	}
}

// TestSearchLimitCaps proves a positive limit caps the result while a limit <= 0 returns
// everything matched.
func TestSearchLimitCaps(t *testing.T) {
	ctx := context.Background()
	s := skill.NewStore(backing(t))
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.Upsert(ctx, state.Skill{Slug: slug, Body: "deploy things"}); err != nil {
			t.Fatalf("upsert %s: %v", slug, err)
		}
	}
	capped, err := s.Search(ctx, "deploy", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(capped) != 2 || capped[0].Slug != "alpha" || capped[1].Slug != "beta" {
		t.Fatalf("limit must cap the slug-ordered result, got %+v", capped)
	}
	all, err := s.Search(ctx, "deploy", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("limit <= 0 must return every match, got %d", len(all))
	}
}
