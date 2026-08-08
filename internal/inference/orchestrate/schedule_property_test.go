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
//
// Each property is checked by its own method on scheduleCase, so a failing run names the
// invariant that broke rather than a line number inside one long body.
func TestScheduleInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		c := newScheduleCase(drawWorkload(rt))
		c.checkLaunchesAreWantedAndAbsent(rt)
		c.checkPinsOverrideTheBudget(rt)
		c.checkEvictionsAreEvictable(rt)
		c.checkKeptLoadFitsTheBudget(rt)
	})
}

// scheduleCase is one drawn workload together with the plan Schedule produced for it. It
// holds the lookups every invariant needs, so each check reads as the property it states
// rather than as the bookkeeping that gets it there.
type scheduleCase struct {
	desired  []Desired
	resident []Resident
	budget   int64
	plan     Plan

	desiredByID  map[string]Desired
	residentByID map[string]Resident
	launched     map[string]bool
	evicted      map[string]bool
}

func newScheduleCase(desired []Desired, resident []Resident, budget int64) scheduleCase {
	p := Schedule(desired, resident, budget)
	byID := make(map[string]Resident, len(resident))
	for _, r := range resident {
		byID[r.ModelID] = r
	}
	return scheduleCase{
		desired:      desired,
		resident:     resident,
		budget:       budget,
		plan:         p,
		desiredByID:  desiredByID(desired),
		residentByID: byID,
		launched:     idSet(p.Launch),
		evicted:      idSet(p.Evict),
	}
}

// footprintOf reports the single footprint the policy charges a model: a desired model at
// its estimate, a resident-only model at its observed size.
func (c scheduleCase) footprintOf(id string) int64 {
	if d, ok := c.desiredByID[id]; ok {
		return nonNeg(d.Footprint)
	}
	return nonNeg(c.residentByID[id].Footprint)
}

// checkLaunchesAreWantedAndAbsent: a launch is only ever for a model somebody asked for and
// that is not already running, because launching either would waste a slot.
func (c scheduleCase) checkLaunchesAreWantedAndAbsent(rt *rapid.T) {
	desiredSet := idSet(desiredIDs(c.desired))
	for _, id := range c.plan.Launch {
		if !desiredSet[id] {
			rt.Fatalf("launched a model that is not desired: %q", id)
		}
		if _, ok := c.residentByID[id]; ok {
			rt.Fatalf("launched a model that is already resident: %q", id)
		}
	}
}

// checkPinsOverrideTheBudget: a pinned desired model is never evicted and never
// unschedulable. A pin is the caller's statement that this model outranks the budget.
func (c scheduleCase) checkPinsOverrideTheBudget(rt *rapid.T) {
	unschedSet := idSet(c.plan.Unschedulable)
	for _, d := range c.desired {
		if !d.Pinned {
			continue
		}
		if unschedSet[d.ModelID] {
			rt.Fatalf("pinned desired model is unschedulable: %q", d.ModelID)
		}
		if c.evicted[d.ModelID] {
			rt.Fatalf("pinned desired model is evicted: %q", d.ModelID)
		}
	}
}

// checkEvictionsAreEvictable: an eviction names a resident model that is neither pinned nor
// serving a request, and the launch and evict sets never overlap.
func (c scheduleCase) checkEvictionsAreEvictable(rt *rapid.T) {
	for _, id := range c.plan.Evict {
		r, ok := c.residentByID[id]
		if !ok {
			rt.Fatalf("evicted a model that is not resident: %q", id)
		}
		if r.Pinned || r.Active {
			rt.Fatalf("evicted a pinned or active model: %q (%+v)", id, r)
		}
		if c.launched[id] {
			rt.Fatalf("model %q is both launched and evicted", id)
		}
	}
}

// checkKeptLoadFitsTheBudget: the kept set (survivors plus launches) stays inside the
// budget, unless the forced load alone already exceeds it. Forced models are the pinned
// desired ones plus pinned or active residents, and none of them can be evicted to make
// room, so the overshoot they cause is the one the budget has to tolerate.
func (c scheduleCase) checkKeptLoadFitsTheBudget(rt *rapid.T) {
	forcedSet := map[string]bool{}
	for _, d := range c.desired {
		if d.Pinned {
			forcedSet[d.ModelID] = true
		}
	}
	for _, r := range c.resident {
		if r.Pinned || r.Active {
			forcedSet[r.ModelID] = true
		}
	}
	var forced int64
	for id := range forcedSet {
		forced += c.footprintOf(id)
	}

	keptIDs := map[string]bool{}
	for _, r := range c.resident {
		if !c.evicted[r.ModelID] {
			keptIDs[r.ModelID] = true
		}
	}
	for _, id := range c.plan.Launch {
		keptIDs[id] = true
	}
	var kept int64
	for id := range keptIDs {
		kept += c.footprintOf(id)
	}
	if kept > c.budget && kept > forced {
		rt.Fatalf("kept footprint %d exceeds budget %d and forced %d", kept, c.budget, forced)
	}
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
