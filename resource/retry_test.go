package resource_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

const retryAPIVersion = "test.flynn.dev/v1"

var retrySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "n": {"type": "integer"}
  }
}`)

func retryStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(resource.Kind{APIVersion: retryAPIVersion, Name: "Counter", Schema: retrySchema}); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func putCounter(t *testing.T, store resource.Store) resource.Resource {
	t.Helper()
	r, err := store.Put(context.Background(), resource.Resource{
		APIVersion: retryAPIVersion,
		Kind:       "Counter",
		Name:       "c",
		Spec:       json.RawMessage(`{"n":0}`),
		Status:     json.RawMessage(`{"count":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type counterStatus struct {
	Count int `json:"count"`
}

func bumpCount(r *resource.Resource) error {
	var s counterStatus
	if err := json.Unmarshal(r.Status, &s); err != nil {
		return err
	}
	s.Count++
	enc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	r.Status = enc
	return nil
}

func TestUpdateAppliesMutation(t *testing.T) {
	store := retryStore(t)
	putCounter(t, store)
	out, err := resource.Update(context.Background(), store, "Counter", resource.Scope{}, "c", bumpCount)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	var s counterStatus
	if err := json.Unmarshal(out.Status, &s); err != nil {
		t.Fatal(err)
	}
	if s.Count != 1 {
		t.Fatalf("count = %d, want 1", s.Count)
	}
}

// TestUpdateConcurrentWritersAllLand is the property the primitive exists for:
// N concurrent read-modify-write increments against one resource must all land,
// no update lost to a version conflict.
func TestUpdateConcurrentWritersAllLand(t *testing.T) {
	store := retryStore(t)
	r := putCounter(t, store)
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := resource.UpdateByID(context.Background(), store, r.ID, bumpCount); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update: %v", err)
	}
	got, err := store.GetByID(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var s counterStatus
	if err := json.Unmarshal(got.Status, &s); err != nil {
		t.Fatal(err)
	}
	if s.Count != writers {
		t.Fatalf("count = %d, want %d (an update was lost)", s.Count, writers)
	}
}

func TestUpdateSkipWritesNothing(t *testing.T) {
	store := retryStore(t)
	r := putCounter(t, store)
	out, err := resource.UpdateByID(context.Background(), store, r.ID, func(*resource.Resource) error {
		return resource.ErrSkipUpdate
	})
	if err != nil {
		t.Fatalf("skip must not be an error, got %v", err)
	}
	if out.SyncVersion != r.SyncVersion {
		t.Fatalf("skip must not write: version moved %d -> %d", r.SyncVersion, out.SyncVersion)
	}
}

func TestUpdateNotFoundPropagates(t *testing.T) {
	store := retryStore(t)
	_, err := resource.Update(context.Background(), store, "Counter", resource.Scope{}, "absent", bumpCount)
	if !errors.Is(err, resource.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateMutateErrorStopsLoop(t *testing.T) {
	store := retryStore(t)
	r := putCounter(t, store)
	boom := errors.New("boom")
	calls := 0
	_, err := resource.UpdateByID(context.Background(), store, r.ID, func(*resource.Resource) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the mutate error", err)
	}
	if calls != 1 {
		t.Fatalf("mutate called %d times, want 1 (no retry on a non-conflict error)", calls)
	}
}

// conflictStore forces every Put to conflict, to exercise retry exhaustion.
type conflictStore struct {
	resource.Store
	puts int
}

func (c *conflictStore) Put(context.Context, resource.Resource) (resource.Resource, error) {
	c.puts++
	return resource.Resource{}, fmt.Errorf("forced: %w", resource.ErrConflict)
}

func TestUpdateExhaustionReportsConflict(t *testing.T) {
	store := retryStore(t)
	r := putCounter(t, store)
	cs := &conflictStore{Store: store}
	_, err := resource.UpdateByID(context.Background(), cs, r.ID, bumpCount)
	if !errors.Is(err, resource.ErrConflict) {
		t.Fatalf("exhaustion must report ErrConflict, got %v", err)
	}
	if cs.puts < 2 {
		t.Fatalf("puts = %d, want the loop to have retried", cs.puts)
	}
}
