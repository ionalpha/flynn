package memory_test

import (
	"context"
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
