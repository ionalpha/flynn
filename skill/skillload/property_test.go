package skillload

import (
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/state"
)

// Whatever the calls and the cap, a run loads no more bodies by trigger than the cap
// allows, loads none twice, and a slug the cap withheld is reported once.
func TestProp_TriggerLoadsRespectTheCapAndNeverRepeat(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		max := rapid.IntRange(1, 4).Draw(rt, "cap")
		n := rapid.IntRange(1, 8).Draw(rt, "skills")
		var cands []Candidate
		for i := 0; i < n; i++ {
			cands = append(cands, Candidate{
				Slug:     string(rune('a' + i)),
				Triggers: []state.SkillTrigger{{Kind: "tool", Match: "shell"}},
			})
		}
		turns := rapid.IntRange(1, 6).Draw(rt, "turns")

		var st State
		loaded := map[string]int{}
		suppressed := map[string]int{}
		for i := 0; i < turns; i++ {
			obs := Observation{Calls: []Call{shell("ls", 0)}}
			d := Decide(Policy{MaxTriggered: max}, st, cands, obs)
			for _, l := range d.Loads {
				loaded[l.Slug]++
			}
			for _, l := range d.Suppressed {
				suppressed[l.Slug]++
			}
			st = d.State
		}
		total := 0
		for slug, c := range loaded {
			total += c
			if c > 1 {
				rt.Fatalf("%s loaded %d times", slug, c)
			}
		}
		if total > max {
			rt.Fatalf("%d bodies loaded by trigger, cap %d", total, max)
		}
		for slug, c := range suppressed {
			if c > 1 {
				rt.Fatalf("%s reported as suppressed %d times", slug, c)
			}
			if loaded[slug] > 0 {
				rt.Fatalf("%s was both loaded and suppressed", slug)
			}
		}
		if !slices.IsSorted(st.Loaded) && len(st.Loaded) != len(loaded) {
			rt.Fatalf("state lists %v, loads were %v", st.Loaded, loaded)
		}
	})
}
