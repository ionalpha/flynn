package orchestrate

import (
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// TestDraftLaunchedWithPrimary proves a desired model and its draft are launched together when
// both fit the budget.
func TestDraftLaunchedWithPrimary(t *testing.T) {
	p := Schedule(
		[]Desired{{ModelID: "main", Footprint: 50, Draft: &Draft{ModelID: "draft", Footprint: 10}}},
		nil, 100,
	)
	if want := []string{"draft", "main"}; !slices.Equal(p.Launch, want) {
		t.Fatalf("launch = %v, want %v", p.Launch, want)
	}
	if len(p.Evict) != 0 || len(p.Unschedulable) != 0 {
		t.Fatalf("unexpected evict/unschedulable: %+v", p)
	}
}

// TestDraftEvictedWithPrimary proves a draft does not outlive its primary: when the primary is no
// longer desired, both leave the resident set.
func TestDraftEvictedWithPrimary(t *testing.T) {
	p := Schedule(
		nil,
		[]Resident{{ModelID: "main", Footprint: 50}, {ModelID: "draft", Footprint: 10}},
		100,
	)
	if want := []string{"draft", "main"}; !slices.Equal(p.Evict, want) {
		t.Fatalf("evict = %v, want %v", p.Evict, want)
	}
}

// TestPairUnschedulableWhenItExceedsBudget proves a primary that fits alone but not with its draft
// is reported unschedulable rather than launched without the draft it needs.
func TestPairUnschedulableWhenItExceedsBudget(t *testing.T) {
	p := Schedule(
		[]Desired{{ModelID: "main", Footprint: 50, Draft: &Draft{ModelID: "draft", Footprint: 60}}},
		nil, 80, // fits main alone (50) but not the pair (110)
	)
	if len(p.Launch) != 0 {
		t.Fatalf("a pair over budget must launch nothing, got %v", p.Launch)
	}
	if want := []string{"main"}; !slices.Equal(p.Unschedulable, want) {
		t.Fatalf("unschedulable = %v, want %v", p.Unschedulable, want)
	}
}

// TestPinnedPrimaryForcesDraftOverBudget proves a pinned model's draft overrides the budget with
// it, since a pinned speculative-decoding pair must stay hot.
func TestPinnedPrimaryForcesDraftOverBudget(t *testing.T) {
	p := Schedule(
		[]Desired{{ModelID: "main", Footprint: 50, Pinned: true, Draft: &Draft{ModelID: "draft", Footprint: 60}}},
		nil, 80,
	)
	if want := []string{"draft", "main"}; !slices.Equal(p.Launch, want) {
		t.Fatalf("launch = %v, want %v", p.Launch, want)
	}
	if len(p.Unschedulable) != 0 {
		t.Fatalf("a pinned pair must not be unschedulable: %v", p.Unschedulable)
	}
}

// TestPairIsAFixedPoint proves a pair already resident and desired yields an empty plan.
func TestPairIsAFixedPoint(t *testing.T) {
	p := Schedule(
		[]Desired{{ModelID: "main", Footprint: 50, Draft: &Draft{ModelID: "draft", Footprint: 10}}},
		[]Resident{{ModelID: "main", Footprint: 50}, {ModelID: "draft", Footprint: 10}},
		100,
	)
	if len(p.Launch) != 0 || len(p.Evict) != 0 {
		t.Fatalf("a resident, desired pair must be a fixed point, got %+v", p)
	}
}

// drawDraftWorkload generates desired models, some paired with a draft from a separate id pool, a
// resident set, and a budget, so the draft co-residency invariant can be checked over arbitrary
// inputs.
func drawDraftWorkload(rt *rapid.T) ([]Desired, []Resident, int64) {
	primaries := []string{"a", "b", "c"}
	drafts := []string{"da", "db", "dc"}

	var desired []Desired
	for i, id := range primaries {
		if !rapid.Bool().Draw(rt, "want:"+id) {
			continue
		}
		d := Desired{
			ModelID:   id,
			Footprint: rapid.Int64Range(0, 80).Draw(rt, "dfp:"+id),
			Priority:  rapid.IntRange(0, 3).Draw(rt, "prio:"+id),
			Pinned:    rapid.Bool().Draw(rt, "pin:"+id),
		}
		if rapid.Bool().Draw(rt, "draft:"+id) {
			d.Draft = &Draft{ModelID: drafts[i], Footprint: rapid.Int64Range(0, 40).Draw(rt, "dffp:"+id)}
		}
		desired = append(desired, d)
	}

	var resident []Resident
	for _, id := range append(append([]string{}, primaries...), drafts...) {
		if rapid.Bool().Draw(rt, "res:"+id) {
			resident = append(resident, Resident{
				ModelID:   id,
				Footprint: rapid.Int64Range(0, 80).Draw(rt, "rfp:"+id),
				Active:    rapid.Bool().Draw(rt, "act:"+id),
				LastUsed:  rapid.Int64Range(0, 100).Draw(rt, "lru:"+id),
			})
		}
	}

	return desired, resident, rapid.Int64Range(0, 300).Draw(rt, "budget")
}

// TestDraftCoResidency is the new headline invariant: in the resulting resident set, a draft is
// present exactly when its primary is, so a speculative-decoding pair is never half-resident. An
// active resident draft is the one exception the policy already makes, kept to protect in-flight
// work, so it is excluded from the equivalence.
func TestDraftCoResidency(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		desired, resident, budget := drawDraftWorkload(rt)
		p := Schedule(desired, resident, budget)

		evicted := idSet(p.Evict)
		kept := map[string]bool{}
		for _, r := range resident {
			if !evicted[r.ModelID] {
				kept[r.ModelID] = true
			}
		}
		for _, id := range p.Launch {
			kept[id] = true
		}

		activeByID := map[string]bool{}
		for _, r := range resident {
			activeByID[r.ModelID] = r.Active
		}
		for _, d := range desired {
			if d.Draft == nil {
				continue
			}
			// A draft kept only because it is actively decoding is allowed even if its primary
			// was dropped; otherwise the two must agree.
			if activeByID[d.Draft.ModelID] && !kept[d.ModelID] {
				continue
			}
			if kept[d.ModelID] != kept[d.Draft.ModelID] {
				rt.Fatalf("pair %q/%q half-resident: primary kept=%v draft kept=%v (plan %+v)",
					d.ModelID, d.Draft.ModelID, kept[d.ModelID], kept[d.Draft.ModelID], p)
			}
		}
	})
}
