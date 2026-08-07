package ridealong_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

func TestPrimeScopeRecordsWhatWasPushed(t *testing.T) {
	ctx := ridealong.NewPrimeScope(context.Background())
	ridealong.MarkPushed(ctx, "mem-b", "mem-a")
	ridealong.MarkPushed(ctx, "mem-a", "", "mem-c")

	if got, want := ridealong.PrimedIDs(ctx), []string{"mem-a", "mem-b", "mem-c"}; !slices.Equal(got, want) {
		t.Errorf("PrimedIDs = %v, want %v (sorted, deduplicated, no empty id)", got, want)
	}
	if !ridealong.Primed(ctx, "mem-a") {
		t.Error("a pushed id is not primed")
	}
	if ridealong.Primed(ctx, "mem-z") {
		t.Error("an id nobody pushed is primed")
	}
	if got := ridealong.OriginFor(ctx, "mem-a"); got != state.UsagePrimed {
		t.Errorf("OriginFor a pushed id = %q, want %q", got, state.UsagePrimed)
	}
	if got := ridealong.OriginFor(ctx, "mem-z"); got != state.UsageOrganic {
		t.Errorf("OriginFor an unpushed id = %q, want %q", got, state.UsageOrganic)
	}
}

func TestNoPrimeScopeReportsOrganic(t *testing.T) {
	ctx := context.Background()
	// A host that has not adopted scopes calls these unconditionally; treating its
	// runs as primed would file every use as a digest effect and hide the signal.
	ridealong.MarkPushed(ctx, "mem-a")
	if ridealong.Primed(ctx, "mem-a") {
		t.Error("an unscoped context reports a primed id")
	}
	if got := ridealong.PrimedIDs(ctx); got != nil {
		t.Errorf("PrimedIDs on an unscoped context = %v, want nil", got)
	}
	if got := ridealong.OriginFor(ctx, "mem-a"); got != state.UsageOrganic {
		t.Errorf("OriginFor = %q, want %q", got, state.UsageOrganic)
	}
}

func TestPrimeScopesDoNotNest(t *testing.T) {
	outer := ridealong.NewPrimeScope(context.Background())
	ridealong.MarkPushed(outer, "mem-a")

	// A subagent given its own scope did not see the parent's digest, so a fact it
	// recalls is a fact it found.
	inner := ridealong.NewPrimeScope(outer)
	if ridealong.Primed(inner, "mem-a") {
		t.Error("an inner scope inherited the outer scope's pushes")
	}
	ridealong.MarkPushed(inner, "mem-b")
	if ridealong.Primed(outer, "mem-b") {
		t.Error("marking an inner scope reached the outer one")
	}
	if got, want := ridealong.PrimedIDs(outer), []string{"mem-a"}; !slices.Equal(got, want) {
		t.Errorf("outer PrimedIDs = %v, want %v", got, want)
	}
}

func TestPrimeScopeIsConcurrencySafe(t *testing.T) {
	ctx := ridealong.NewPrimeScope(context.Background())
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			ridealong.MarkPushed(ctx, id)
			ridealong.Primed(ctx, id)
			ridealong.PrimedIDs(ctx)
		}()
	}
	wg.Wait()
	if got := len(ridealong.PrimedIDs(ctx)); got != 16 {
		t.Errorf("primed %d ids, want 16", got)
	}
}
