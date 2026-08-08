package skillab_test

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillab"
)

// The verdict is symmetric. Swapping which arm each pair was in swaps helped for
// hurt, leaves the significance where it was, and negates the effect size. A test
// that is easier to pass in one direction than the other would let a harness built
// by the people who wrote the skills flatter them.
func TestProp_TheVerdictIsSymmetricUnderSwappingTheArms(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 20).Draw(rt, "exercises")
		with := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "with")
		without := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "without")

		forward := measureFrom(rt, with, without)
		swapped := measureFrom(rt, without, with)

		if forward.P != swapped.P {
			rt.Fatalf("p changed when the arms swapped: %v then %v", forward.P, swapped.P)
		}
		if forward.Gain != -swapped.Gain {
			rt.Fatalf("gain %v did not negate: %v", forward.Gain, swapped.Gain)
		}
		want := map[skillab.Verdict]skillab.Verdict{
			skillab.Helped:       skillab.Hurt,
			skillab.Hurt:         skillab.Helped,
			skillab.NoDifference: skillab.NoDifference,
		}[forward.Verdict]
		if swapped.Verdict != want {
			rt.Fatalf("%q swapped to %q, want %q", forward.Verdict, swapped.Verdict, want)
		}
	})
}

// Pairs the two arms agreed on carry no information about the difference between
// them, so adding any number of them moves neither the significance nor the verdict.
// This is the property that makes a small measurement trustworthy: it says the
// harness cannot be talked into a result by padding the set with exercises that both
// arms pass.
func TestProp_AgreeingPairsDoNotMoveTheVerdict(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(rt, "exercises")
		with := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "with")
		without := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "without")
		padded := rapid.IntRange(0, 10).Draw(rt, "agreeing pairs")
		agree := rapid.Bool().Draw(rt, "agree by passing")

		base := measureFrom(rt, with, without)
		for range padded {
			with = append(with, agree)
			without = append(without, agree)
		}
		grown := measureFrom(rt, with, without)

		if base.P != grown.P {
			rt.Fatalf("p moved from %v to %v on %d agreeing pairs", base.P, grown.P, padded)
		}
		if base.Verdict != grown.Verdict {
			rt.Fatalf("verdict moved from %q to %q on %d agreeing pairs", base.Verdict, grown.Verdict, padded)
		}
	})
}

// Whatever the outcomes, the report's tallies are consistent with its pairs: the
// counts add up, the discordant halves never exceed the total, and the probability
// stays a probability. A verdict is read off these, so a report that contradicted
// itself would be read as fact.
func TestProp_TheReportAgreesWithItsOwnPairs(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 25).Draw(rt, "exercises")
		with := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "with")
		without := rapid.SliceOfN(rapid.Bool(), n, n).Draw(rt, "without")
		rep := measureFrom(rt, with, without)

		if len(rep.Pairs) != n {
			rt.Fatalf("%d pairs from %d exercises", len(rep.Pairs), n)
		}
		var wantWith, wantWithout, helped, hurt int
		for i := range n {
			if with[i] {
				wantWith++
			}
			if without[i] {
				wantWithout++
			}
			if with[i] && !without[i] {
				helped++
			}
			if !with[i] && without[i] {
				hurt++
			}
		}
		if rep.WithPasses != wantWith || rep.WithoutPasses != wantWithout {
			rt.Fatalf("passes %d/%d, want %d/%d", rep.WithPasses, rep.WithoutPasses, wantWith, wantWithout)
		}
		if rep.HelpedOnly != helped || rep.HurtOnly != hurt {
			rt.Fatalf("discordant %d/%d, want %d/%d", rep.HelpedOnly, rep.HurtOnly, helped, hurt)
		}
		if rep.HelpedOnly+rep.HurtOnly > len(rep.Pairs) {
			rt.Fatalf("more discordant pairs than pairs")
		}
		if rep.P < 0 || rep.P > 1 {
			rt.Fatalf("p = %v, outside [0,1]", rep.P)
		}
		if rep.Uninformative() && rep.Verdict != skillab.NoDifference {
			rt.Fatalf("a set that never disagreed returned %q", rep.Verdict)
		}
	})
}

// measureFrom runs a measurement whose outcomes are dictated, one exercise per element.
func measureFrom(rt *rapid.T, with, without []bool) skillab.Report {
	rt.Helper()
	s := skillab.Set{Skill: "under-test"}
	for i := range with {
		s.Exercises = append(s.Exercises, skillab.Exercise{Objective: fmt.Sprintf("exercise %d", i), Verify: "exit 0", Line: i + 1})
	}
	i := 0
	rep, err := skillab.Measure(context.Background(), s, 1, func(_ context.Context, _ skillab.Exercise, _ int, withSkill bool) (bool, error) {
		defer func() {
			if !withSkill {
				i++
			}
		}()
		if withSkill {
			return with[i], nil
		}
		return without[i], nil
	})
	if err != nil {
		rt.Fatalf("measure: %v", err)
	}
	return rep
}
