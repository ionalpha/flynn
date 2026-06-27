package orchestrate

import (
	"testing"

	"pgregory.net/rapid"
)

// drawWorkload generates an arbitrary desired set, resident set, and budget. Model ids are
// drawn from a small shared pool so the two sets overlap, which is where the interesting
// cases (a desired model already resident, a resident model no longer desired) live.
func drawWorkload(rt *rapid.T) ([]Desired, []Resident, int64) {
	ids := []string{"a", "b", "c", "d", "e"}

	desired := make([]Desired, 0, len(ids))
	for _, id := range ids {
		if rapid.Bool().Draw(rt, "want:"+id) {
			desired = append(desired, Desired{
				ModelID:   id,
				Footprint: rapid.Int64Range(0, 100).Draw(rt, "dfp:"+id),
				Priority:  rapid.IntRange(0, 5).Draw(rt, "prio:"+id),
				Pinned:    rapid.Bool().Draw(rt, "dpin:"+id),
			})
		}
	}

	resident := make([]Resident, 0, len(ids))
	for _, id := range ids {
		if rapid.Bool().Draw(rt, "res:"+id) {
			resident = append(resident, Resident{
				ModelID:   id,
				Footprint: rapid.Int64Range(0, 100).Draw(rt, "rfp:"+id),
				Pinned:    rapid.Bool().Draw(rt, "rpin:"+id),
				Active:    rapid.Bool().Draw(rt, "act:"+id),
				LastUsed:  rapid.Int64Range(0, 1000).Draw(rt, "lru:"+id),
			})
		}
	}

	budget := rapid.Int64Range(0, 300).Draw(rt, "budget")
	return desired, resident, budget
}

// TestScheduleInvariants asserts the universal safety and consistency properties of a plan,
// for any workload: launches are desired and not already resident, evictions are resident
// and neither pinned nor active, the two never overlap, and the kept set never exceeds the
// budget unless forced (pinned or active) load already does.
func TestScheduleInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		desired, resident, budget := drawWorkload(rt)
		p := Schedule(desired, resident, budget)

		desiredSet := idSet(desiredIDs(desired))
		residentByID := map[string]Resident{}
		for _, r := range resident {
			residentByID[r.ModelID] = r
		}

		launchSet := idSet(p.Launch)
		for _, id := range p.Launch {
			if !desiredSet[id] {
				rt.Fatalf("launched a model that is not desired: %q", id)
			}
			if _, ok := residentByID[id]; ok {
				rt.Fatalf("launched a model that is already resident: %q", id)
			}
		}

		// A pinned desired model is never evicted and never unschedulable: a pin overrides
		// the budget.
		evictedSet := idSet(p.Evict)
		unschedSet := idSet(p.Unschedulable)
		for _, d := range desired {
			if d.Pinned {
				if unschedSet[d.ModelID] {
					rt.Fatalf("pinned desired model is unschedulable: %q", d.ModelID)
				}
				if evictedSet[d.ModelID] {
					rt.Fatalf("pinned desired model is evicted: %q", d.ModelID)
				}
			}
		}
		for _, id := range p.Evict {
			r, ok := residentByID[id]
			if !ok {
				rt.Fatalf("evicted a model that is not resident: %q", id)
			}
			if r.Pinned || r.Active {
				rt.Fatalf("evicted a pinned or active model: %q (%+v)", id, r)
			}
			if launchSet[id] {
				rt.Fatalf("model %q is both launched and evicted", id)
			}
		}

		// Budget invariant: the kept set (forced load + admitted models) does not exceed the
		// budget, unless the forced load alone already exceeds it (which cannot be evicted).
		// Footprint is single-valued per model, matching the policy: a desired model at its
		// estimate, a resident-only model at its observed size.
		dByID := desiredByID(desired)
		footOf := func(id string) int64 {
			if d, ok := dByID[id]; ok {
				return nonNeg(d.Footprint)
			}
			return nonNeg(residentByID[id].Footprint)
		}
		// Forced models (pinned desired, plus pinned or active residents) are kept regardless
		// of budget, so the kept footprint may exceed the budget only up to the forced load.
		forcedSet := map[string]bool{}
		for _, d := range desired {
			if d.Pinned {
				forcedSet[d.ModelID] = true
			}
		}
		for _, r := range resident {
			if r.Pinned || r.Active {
				forcedSet[r.ModelID] = true
			}
		}
		var forced int64
		for id := range forcedSet {
			forced += footOf(id)
		}

		evicted := idSet(p.Evict)
		keptIDs := map[string]bool{}
		for _, r := range resident {
			if !evicted[r.ModelID] {
				keptIDs[r.ModelID] = true
			}
		}
		for _, id := range p.Launch {
			keptIDs[id] = true
		}
		var kept int64
		for id := range keptIDs {
			kept += footOf(id)
		}
		if kept > budget && kept > forced {
			rt.Fatalf("kept footprint %d exceeds budget %d and forced %d", kept, budget, forced)
		}
	})
}

// TestScheduleIsAFixedPoint asserts the chosen set is stable: applying a plan and scheduling
// again produces no further launches or evictions, so the loop converges in one step and
// never oscillates.
func TestScheduleIsAFixedPoint(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		desired, resident, budget := drawWorkload(rt)
		first := Schedule(desired, resident, budget)
		next := applyPlan(desired, resident, first)
		second := Schedule(desired, next, budget)
		if len(second.Launch) != 0 || len(second.Evict) != 0 {
			rt.Fatalf("not a fixed point: second plan launches %v evicts %v", second.Launch, second.Evict)
		}
	})
}

// applyPlan simulates the serve manager carrying out a plan: evicted models leave the
// resident set, surviving residents are unchanged, and launched models join it with their
// desired footprint and pin, idle and freshly used.
func applyPlan(desired []Desired, resident []Resident, p Plan) []Resident {
	dByID := desiredByID(desired)
	evicted := idSet(p.Evict)
	var out []Resident
	for _, r := range resident {
		if !evicted[r.ModelID] {
			out = append(out, r)
		}
	}
	for _, id := range p.Launch {
		d := dByID[id]
		out = append(out, Resident{ModelID: id, Footprint: d.Footprint, Pinned: d.Pinned})
	}
	return out
}

func desiredIDs(ds []Desired) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ModelID
	}
	return out
}

func desiredByID(ds []Desired) map[string]Desired {
	out := make(map[string]Desired, len(ds))
	for _, d := range ds {
		out[d.ModelID] = d
	}
	return out
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func nonNeg(b int64) int64 {
	if b < 0 {
		return 0
	}
	return b
}
