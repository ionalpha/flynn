package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// newResourceStore builds a bare resource store with the memory kinds registered,
// so a test can put more than one facade over the same data.
func newResourceStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatalf("register core kinds: %v", err)
	}
	if err := memory.RegisterKind(reg); err != nil {
		t.Fatalf("register memory kinds: %v", err)
	}
	return resource.NewMemory(reg)
}

// TestUsageRefusesAnInstanceMismatch pins the guard on the one option that can be
// set wrong. A usage record is keyed by the instance it belongs to, so a facade
// naming itself something the store does not stamp would file this instance's
// reads under a name nothing else reads, and every fleet-wide total would come
// back short without anything failing.
func TestUsageRefusesAnInstanceMismatch(t *testing.T) {
	ctx := context.Background()
	rs := newResourceStore(t)
	written, err := memory.NewStore(rs).Write(ctx, state.MemoryItem{Kind: "fact", Content: "counted"})
	if err != nil {
		t.Fatal(err)
	}

	mismatched := memory.NewStore(rs, memory.WithInstanceID("somebody-else"))
	err = mismatched.RecordPush(ctx, []string{written.ID})
	if !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("RecordPush under a mismatched instance = %v, want ErrInvalid", err)
	}
}

// TestUsageReportsAStoreFailure pins the error paths: a resource store that fails
// is reported, not swallowed. A usage read that answered "no rows" when the store
// was refusing to talk would look exactly like a corpus nobody has touched, which
// is the reading a selection policy would act on.
func TestUsageReportsAStoreFailure(t *testing.T) {
	ctx := context.Background()
	rs := newResourceStore(t)
	written, err := memory.NewStore(rs).Write(ctx, state.MemoryItem{Kind: "fact", Content: "counted"})
	if err != nil {
		t.Fatal(err)
	}

	broken := memory.NewStore(&failingStore{Store: rs, fail: errors.New("store is down")})
	if _, err := broken.Usage(ctx, nil); err == nil {
		t.Fatal("Usage over a failing store reported no error")
	}
	if err := broken.RecordPush(ctx, []string{written.ID}); err == nil {
		t.Fatal("RecordPush over a failing store reported no error")
	}
	if err := broken.RecordUse(ctx, written.ID, state.UsageOrganic); err == nil {
		t.Fatal("RecordUse over a failing store reported no error")
	}

	// A Get that fails for a reason other than "not found" is a failure, not an
	// invitation to start the counter over from zero.
	getBroken := memory.NewStore(&failingStore{Store: rs, failGet: errors.New("store is down")})
	if err := getBroken.RecordUse(ctx, written.ID, state.UsageOrganic); err == nil {
		t.Fatal("RecordUse over a store whose Get fails reported no error")
	}
	// A write that fails for a reason other than a conflict is not retried: the
	// retry exists for a contended record, not for a store that is refusing writes.
	putBroken := memory.NewStore(&failingStore{Store: rs, failPut: errors.New("store is down")})
	if err := putBroken.RecordPush(ctx, []string{written.ID}); err == nil {
		t.Fatal("RecordPush over a store whose Put fails reported no error")
	}
}

// TestUsageRejectsAnUndecodableRecord pins what happens when a stored record does
// not fit the typed shape: the read fails rather than reporting the counters as
// zero, which would read as an item nobody has ever pushed.
func TestUsageRejectsAnUndecodableRecord(t *testing.T) {
	ctx := context.Background()
	rs := newResourceStore(t)
	facade := memory.NewStore(rs)
	written, err := facade.Write(ctx, state.MemoryItem{Kind: "fact", Content: "counted"})
	if err != nil {
		t.Fatal(err)
	}
	// A count too large for the typed field: a JSON number, so the kind's schema
	// admits it, and not an int64, so decoding it does not.
	if _, err := rs.Put(ctx, resource.Resource{
		APIVersion: "memory.ionagent.io/v1",
		Kind:       memory.UsageKind,
		Name:       "memuse-" + written.ID + "-local",
		Spec:       json.RawMessage(`{"memory_id":"` + written.ID + `","instance_id":"local","push_count":99999999999999999999}`),
	}); err != nil {
		t.Fatalf("seed an undecodable usage record: %v", err)
	}
	if _, err := facade.Usage(ctx, nil); err == nil {
		t.Fatal("Usage decoded a record that does not fit the typed shape")
	}
	if err := facade.RecordUse(ctx, written.ID, state.UsageOrganic); err == nil {
		t.Fatal("RecordUse overwrote a record it could not read back")
	}
}

// failingStore fails the resource operations usage depends on. failGet narrows it
// to the read half, so a test can separate "the store is gone" from "this record
// is not there yet", which usage treats very differently.
type failingStore struct {
	resource.Store
	fail    error
	failGet error
	failPut error
}

func (f *failingStore) ListAll(ctx context.Context, kind string, sel resource.Selector) ([]resource.Resource, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.Store.ListAll(ctx, kind, sel)
}

func (f *failingStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if err := firstErr(f.fail, f.failGet); err != nil {
		return resource.Resource{}, err
	}
	return f.Store.Get(ctx, kind, scope, name)
}

func (f *failingStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if err := firstErr(f.fail, f.failPut); err != nil {
		return resource.Resource{}, err
	}
	return f.Store.Put(ctx, r)
}

// firstErr returns the first non-nil of the two errors.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// conflictingPuts fails the first n Put calls with a conflict, then delegates. It
// stands in for another goroutine on this instance recording usage at the same
// moment, which is the only writer a usage record can ever contend with.
type conflictingPuts struct {
	resource.Store
	fail int
}

func (c *conflictingPuts) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if c.fail > 0 {
		c.fail--
		return resource.Resource{}, resource.ErrConflict
	}
	return c.Store.Put(ctx, r)
}

func TestUsageRetriesAConflictingWrite(t *testing.T) {
	ctx := context.Background()
	rs := newResourceStore(t)
	written, err := memory.NewStore(rs).Write(ctx, state.MemoryItem{Kind: "fact", Content: "contended"})
	if err != nil {
		t.Fatal(err)
	}

	// One loser re-reads and reapplies, so the increment lands rather than being
	// lost or reported as an error the caller has to understand.
	retrying := memory.NewStore(&conflictingPuts{Store: rs, fail: 1})
	if err := retrying.RecordUse(ctx, written.ID, state.UsageOrganic); err != nil {
		t.Fatalf("RecordUse against one conflict = %v, want it retried", err)
	}
	rows, err := retrying.Usage(ctx, []string{written.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].OrganicUses != 1 {
		t.Fatalf("usage after a retried write = %+v, want one organic use", rows)
	}

	// A store that keeps conflicting is reporting a real problem, so the retry
	// gives up and says so rather than spinning.
	stuck := memory.NewStore(&conflictingPuts{Store: rs, fail: 99})
	if err := stuck.RecordPush(ctx, []string{written.ID}); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("RecordPush against a store that never settles = %v, want ErrConflict", err)
	}
	rows, err = retrying.Usage(ctx, []string{written.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PushCount != 0 {
		t.Fatalf("usage after a failed push = %+v, want nothing recorded", rows)
	}
}
