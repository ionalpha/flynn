package orchestrate

import "testing"

// FuzzSchedule derives an arbitrary workload from raw bytes and asserts Schedule never
// panics and always returns a plan that satisfies its safety invariants: a launch is never
// also an eviction, an eviction is never a pinned or active model, and the plan is a fixed
// point. Each byte group describes one model; the high bits choose whether it is desired,
// resident, pinned, or active, so a hostile or malformed spec is exercised directly.
func FuzzSchedule(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0x10, 0x01, 0x40})
	f.Add([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})

	f.Fuzz(func(t *testing.T, data []byte) {
		var desired []Desired
		var resident []Resident
		residentSeen := map[string]bool{}
		desiredSeen := map[string]bool{}

		for i := 0; i+1 < len(data); i += 2 {
			id := string(rune('a' + int(data[i])%8))
			flags := data[i+1]
			fp := int64(data[i]) // arbitrary, including small footprints
			if flags&0x01 != 0 && !desiredSeen[id] {
				desiredSeen[id] = true
				desired = append(desired, Desired{
					ModelID:   id,
					Footprint: fp,
					Priority:  int(flags>>4) & 0x07,
					Pinned:    flags&0x02 != 0,
				})
			}
			if flags&0x04 != 0 && !residentSeen[id] {
				residentSeen[id] = true
				resident = append(resident, Resident{
					ModelID:   id,
					Footprint: fp,
					Pinned:    flags&0x08 != 0,
					Active:    flags&0x20 != 0,
					LastUsed:  int64(data[i]),
				})
			}
		}
		budget := int64(0)
		if len(data) > 0 {
			budget = int64(data[0]) * 2
		}

		p := Schedule(desired, resident, budget)

		residentByID := map[string]Resident{}
		for _, r := range resident {
			residentByID[r.ModelID] = r
		}
		launch := idSet(p.Launch)
		for _, id := range p.Evict {
			r := residentByID[id]
			if r.Pinned || r.Active {
				t.Fatalf("evicted a pinned or active model: %q", id)
			}
			if launch[id] {
				t.Fatalf("model both launched and evicted: %q", id)
			}
		}

		// Fixed-point: scheduling again after applying the plan changes nothing.
		next := applyPlan(desired, resident, p)
		second := Schedule(desired, next, budget)
		if len(second.Launch) != 0 || len(second.Evict) != 0 {
			t.Fatalf("not a fixed point: %+v then %+v", p, second)
		}
	})
}
