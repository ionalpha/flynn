package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

var errBrokenQueue = errors.New("queue is broken")

// brokenLeases fails every call that touches another claimant's lease.
type brokenLeases struct{ jobs.Queue }

func (brokenLeases) Recover(context.Context) (int, error) { return 0, errBrokenQueue }

func (brokenLeases) ReleasePayload(context.Context, string, []byte) (int, error) {
	return 0, errBrokenQueue
}

// brokenGoals fails listing, or writing, as asked.
type brokenGoals struct {
	resource.Store
	list, put bool
}

func (s brokenGoals) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	if s.list {
		return nil, errBrokenQueue
	}
	return s.Store.List(ctx, kind, scope, sel)
}

func (s brokenGoals) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if s.put {
		return resource.Resource{}, errBrokenQueue
	}
	return s.Store.Put(ctx, r)
}

// An adoption that cannot read or write the goals reports it.
func TestAdoptReportsAStoreFailure(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "stored", Spec: raw,
	}); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []brokenGoals{{Store: store, list: true}, {Store: store, put: true}} {
		rt, err := New(Config{
			Executor: stubExec{}, Stop: stubStop{}, Store: broken, Jobs: jobs.NewMemory(),
			Confinement: &goal.Confinement{Workdir: t.TempDir()},
		})
		if err != nil {
			t.Fatal(err)
		}
		if adopted, err := rt.Adopt(ctx, "stored"); adopted || !errors.Is(err, errBrokenQueue) {
			t.Errorf("Adopt (list=%t put=%t) = %t, %v, want the store failure", broken.list, broken.put, adopted, err)
		}
	}
}

// unreadable returns the goal with this id with a spec that does not decode.
type unreadable struct {
	resource.Store
	id string
}

func (s unreadable) GetByID(ctx context.Context, id string) (resource.Resource, error) {
	r, err := s.Store.GetByID(ctx, id)
	if id == s.id {
		r.Spec = []byte("7")
	}
	return r, err
}

// Adoption keeps a record a child already has, and reports a child it cannot read.
func TestAdoptKeepsAChildsRecordAndReportsAnUnreadableChild(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	put := func(name, extra string, owner *resource.Resource) resource.Resource {
		spec := `{"objective":"o","stopCondition":"c"` + extra + `}`
		r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name, Spec: []byte(spec)}
		if owner != nil {
			r.OwnerReferences = []resource.OwnerReference{{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: owner.Name, ID: owner.ID}}
		}
		saved, err := store.Put(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	rt, err := New(Config{
		Executor: stubExec{}, Stop: stubStop{}, Store: store, Jobs: jobs.NewMemory(),
		Confinement: &goal.Confinement{Workdir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}

	root := put("root", "", nil)
	put("kept", `,"confinement":{"workdir":"/earlier"}`, &root)
	if adopted, err := rt.Adopt(ctx, "root"); !adopted || err != nil {
		t.Fatalf("Adopt = %t, %v", adopted, err)
	}
	kept, err := store.Get(ctx, goal.Kind, resource.Scope{}, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if spec, err := goal.DecodeSpec(kept); err != nil || spec.Confinement.Workdir != "/earlier" {
		t.Fatalf("child record = %+v, %v, want it unchanged", spec.Confinement, err)
	}

	bad := put("bad-root", "", nil)
	child := put("bad", "", &bad)
	rt.store = unreadable{Store: store, id: child.ID}
	if adopted, err := rt.Adopt(ctx, "bad-root"); adopted || err == nil {
		t.Fatalf("Adopt over an unreadable child = %t, %v, want an error", adopted, err)
	}
}

// A lease that cannot be released or recovered is reported, never skipped.
func TestRuntimeReportsAQueueThatCannotFreeLeases(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	cfg := Config{Executor: stubExec{}, Stop: stubStop{}, Store: store, Jobs: brokenLeases{jobs.NewMemory()}}

	every, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := every.Start(ctx); !errors.Is(err, errBrokenQueue) {
		t.Errorf("Start = %v, want the recover failure", err)
	}

	cfg.DriveSubmittedOnly = true
	own, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "stored", Spec: raw,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := own.Resume(ctx, "stored"); !errors.Is(err, errBrokenQueue) {
		t.Errorf("Resume = %v, want the release failure", err)
	}
}
