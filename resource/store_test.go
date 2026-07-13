package resource_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

const ownerAPIVersion = "gc.test.ionagent.io/v1"

// gcStore is a store with the two kinds the garbage-collection predicate walks: an
// owner and the resource it owns.
func gcStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	for _, name := range []string{"Owner", "Owned"} {
		if err := reg.Register(resource.Kind{APIVersion: ownerAPIVersion, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	store := resource.NewMemory(reg)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ownedBy renders a child carrying a controller owner reference to owner.
func ownedBy(name string, owner resource.Resource) resource.Resource {
	return resource.Resource{
		APIVersion: ownerAPIVersion, Kind: "Owned", Name: name,
		Envelope: resource.Envelope{
			OwnerReferences: []resource.OwnerReference{{
				APIVersion: ownerAPIVersion, Kind: "Owner",
				Name: owner.Name, ID: owner.ID, Controller: true,
			}},
		},
	}
}

// TestOwnerGoneReapsOrphansOnly gates the garbage-collection predicate: a resource
// is an orphan exactly when its controller owner has been deleted or is itself
// terminating. A root (no controller owner) is never an orphan, and a live owner
// keeps its children.
func TestOwnerGoneReapsOrphansOnly(t *testing.T) {
	ctx := context.Background()
	store := gcStore(t)

	owner, err := store.Put(ctx, resource.Resource{APIVersion: ownerAPIVersion, Kind: "Owner", Name: "parent"})
	if err != nil {
		t.Fatal(err)
	}

	// A root: no controller owner at all.
	root := resource.Resource{APIVersion: ownerAPIVersion, Kind: "Owned", Name: "root"}
	if gone, err := resource.OwnerGone(ctx, store, root); err != nil || gone {
		t.Fatalf("OwnerGone(root) = %v, %v; want false, nil (a root is never an orphan)", gone, err)
	}

	// A non-controller owner reference does not make the resource owned.
	weak := root
	weak.OwnerReferences = []resource.OwnerReference{{
		APIVersion: ownerAPIVersion, Kind: "Owner", Name: "ghost", ID: "no-such-id",
	}}
	if gone, err := resource.OwnerGone(ctx, store, weak); err != nil || gone {
		t.Fatalf("OwnerGone(non-controller ref) = %v, %v; want false, nil", gone, err)
	}

	// A live owner keeps its child.
	child := ownedBy("child", owner)
	if gone, err := resource.OwnerGone(ctx, store, child); err != nil || gone {
		t.Fatalf("OwnerGone(live owner) = %v, %v; want false, nil", gone, err)
	}

	// An owner that no longer exists orphans the child.
	dangling := ownedBy("dangling", resource.Resource{Name: "gone", ID: "no-such-id"})
	if gone, err := resource.OwnerGone(ctx, store, dangling); err != nil || !gone {
		t.Fatalf("OwnerGone(deleted owner) = %v, %v; want true, nil", gone, err)
	}
}

// TestOwnerGoneCascadesFromATerminatingOwner is the cascade rule: once an owner is
// terminating (a delete requested while its finalizers still run), its children are
// already orphans, so the subtree is reaped before the owner's own cleanup
// completes rather than after.
func TestOwnerGoneCascadesFromATerminatingOwner(t *testing.T) {
	ctx := context.Background()
	store := gcStore(t)

	owner, err := store.Put(ctx, resource.Resource{
		APIVersion: ownerAPIVersion, Kind: "Owner", Name: "parent",
		Envelope: resource.Envelope{Finalizers: []string{"cleanup"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := ownedBy("child", owner)
	if gone, err := resource.OwnerGone(ctx, store, child); err != nil || gone {
		t.Fatalf("a live owner must not orphan its child: gone=%v err=%v", gone, err)
	}

	// The delete marks the owner terminating: it stays live (its finalizer has not
	// run) but its children are already orphans.
	if err := store.Delete(ctx, "Owner", resource.Scope{}, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "Owner", resource.Scope{}, "parent"); err != nil {
		t.Fatalf("a terminating owner must still be readable: %v", err)
	}
	gone, err := resource.OwnerGone(ctx, store, child)
	if err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Fatal("a terminating owner must cascade the reap to its children")
	}
}

// erroringStore fails every lookup by ID with a backend error that is not
// ErrNotFound, so a caller can prove OwnerGone propagates it instead of reporting
// a false orphan.
type erroringStore struct {
	resource.Store
	err error
}

func (s erroringStore) GetByID(context.Context, string) (resource.Resource, error) {
	return resource.Resource{}, s.err
}

// TestOwnerGonePropagatesBackendErrors closes the dangerous failure mode: a
// backend error must never be read as "the owner is gone", which would let a
// transient read failure delete a live subtree.
func TestOwnerGonePropagatesBackendErrors(t *testing.T) {
	boom := errors.New("backend unavailable")
	store := erroringStore{Store: gcStore(t), err: boom}
	child := ownedBy("child", resource.Resource{Name: "parent", ID: "owner-id"})
	gone, err := resource.OwnerGone(context.Background(), store, child)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the backend error", err)
	}
	if gone {
		t.Fatal("a backend error must never be reported as an orphan")
	}
}

// TestDecodeResourceRejectsMalformedPayloads gates the projection boundary every
// durable backend decodes through: a payload that is not a resource record must
// error rather than project a zero record over live state.
func TestDecodeResourceRejectsMalformedPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"resource is not an object", map[string]any{"resource": "nope"}},
		{"resource is not encodable", map[string]any{"resource": make(chan int)}},
		{"field of the wrong type", map[string]any{"resource": map[string]any{"Name": 7}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resource.DecodeResource(tc.payload); err == nil {
				t.Fatal("DecodeResource must reject a payload that is not a resource record")
			}
		})
	}
}

// TestKindResourceRendersAndRoundTrips is the meta-circular base case: a Kind
// renders as a Resource of kind "Kind" whose spec satisfies the Kind schema, so a
// kind definition is stored, admitted, and read back through the same store as
// everything else. Optional display fields are carried; omitted ones do not appear.
func TestKindResourceRendersAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	t.Cleanup(func() { _ = store.Close() })

	full := resource.Kind{
		APIVersion: "skill.test/v1",
		Name:       "Skill",
		Schema:     json.RawMessage(`{"type":"object"}`),
		Singular:   "skill",
		Plural:     "skills",
	}
	r, err := resource.KindResource(full, resource.Scope{Project: "p"})
	if err != nil {
		t.Fatalf("KindResource: %v", err)
	}
	if r.Kind != resource.KindKind || r.APIVersion != resource.CoreGroupVersion {
		t.Fatalf("a rendered kind must be a core Kind resource, got %s/%s", r.APIVersion, r.Kind)
	}
	if r.Name != "Skill" || r.Scope.Project != "p" {
		t.Fatalf("rendered kind addressed as %q in %+v", r.Name, r.Scope)
	}
	var spec map[string]any
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"apiVersion", "name", "schema", "singular", "plural"} {
		if _, ok := spec[key]; !ok {
			t.Fatalf("rendered kind spec is missing %q: %s", key, r.Spec)
		}
	}
	// It is admitted by the Kind schema and stored like any other resource.
	stored, err := store.Put(ctx, r)
	if err != nil {
		t.Fatalf("a rendered kind must satisfy the Kind schema: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("a stored kind must be assigned an id")
	}

	// The optional display fields are omitted when unset, so the spec stays minimal.
	minimal, err := resource.KindResource(resource.Kind{APIVersion: "skill.test/v1", Name: "Bare"}, resource.Scope{})
	if err != nil {
		t.Fatalf("KindResource: %v", err)
	}
	var bare map[string]any
	if err := json.Unmarshal(minimal.Spec, &bare); err != nil {
		t.Fatal(err)
	}
	if len(bare) != 2 {
		t.Fatalf("a bare kind spec must carry only apiVersion and name, got %s", minimal.Spec)
	}
	if _, err := store.Put(ctx, minimal); err != nil {
		t.Fatalf("a bare kind must still be admitted: %v", err)
	}
}

// TestMergeOutcomeString locks the human-readable form of every outcome, including
// an out-of-range value, so a log line about a merge is never an opaque integer.
func TestMergeOutcomeString(t *testing.T) {
	cases := []struct {
		outcome resource.MergeOutcome
		want    string
	}{
		{resource.MergeApplied, "applied"},
		{resource.MergeIgnored, "ignored"},
		{resource.MergeUnchanged, "unchanged"},
		{resource.MergeOutcome(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.outcome.String(); got != tc.want {
			t.Fatalf("MergeOutcome(%d).String() = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
