package sandbox

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

// outsideTrust draws a Trust that this binary does not define.
func outsideTrust() *rapid.Generator[Trust] {
	return rapid.Map(rapid.OneOf(
		rapid.IntRange(math.MinInt32, int(TrustTrusted)-1),
		rapid.IntRange(int(TrustUntrusted)+1, math.MaxInt32),
	), func(n int) Trust { return Trust(n) })
}

// TestFailClosedTrust proves a trust level this binary does not define is treated as
// untrusted: it requires the hardware boundary, a tier that cannot provide one refuses
// it, and it is never named "trusted". The zero Trust is the exception by design and is
// pinned here so a change to it is a decision rather than an accident: an action that
// declares no trust is the agent's own tool (see dispatch.Action.Trust).
func TestFailClosedTrust(t *testing.T) {
	t.Run("zero is the agent's own tool by design", func(t *testing.T) {
		var zero Trust
		if zero != TrustTrusted {
			t.Fatalf("the zero Trust is %v, documented as TrustTrusted", zero)
		}
	})
	t.Run("unknown levels require the hardware boundary", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			tr := outsideTrust().Draw(rt, "trust")
			if got := Required(tr); got != ContainmentMicroVM {
				rt.Fatalf("Required(%d) = %v, want %v", int(tr), got, ContainmentMicroVM)
			}
			for _, weak := range []Containment{ContainmentNone, ContainmentKernel, ContainmentContainer, ContainmentUserKernel} {
				if Admit(tier{weak}, tr) == nil {
					rt.Fatalf("Admit let trust %d run in %v", int(tr), weak)
				}
			}
			if Admit(tier{ContainmentMicroVM}, tr) != nil {
				rt.Fatalf("a microvm must still admit trust %d: refusing everything is not a policy", int(tr))
			}
			if tr.String() == TrustTrusted.String() {
				rt.Fatalf("trust %d is named %q, which reads as the permissive level", int(tr), tr.String())
			}
		})
	})
	t.Run("select refuses an unknown level no tier can contain", func(t *testing.T) {
		if _, err := Select(Trust(99), tier{ContainmentNone}, tier{ContainmentKernel}); err == nil {
			t.Fatal("Select returned a tier for a trust level it cannot contain")
		}
	})
}

// TestFailClosedContainment proves a sandbox that declares no level, or a level outside
// the scale, is the weakest tier. A level above [ContainmentRemote] would otherwise
// satisfy every requirement, and a negative one is not a stronger or weaker tier but a
// corrupted declaration.
func TestFailClosedContainment(t *testing.T) {
	t.Run("zero is the weakest tier", func(t *testing.T) {
		var zero Containment
		if zero != ContainmentNone {
			t.Fatalf("the zero Containment is %v, want %v", zero, ContainmentNone)
		}
	})
	t.Run("an undeclared sandbox is the weakest tier", func(t *testing.T) {
		if got := ContainmentOf(undeclared{}); got != ContainmentNone {
			t.Fatalf("ContainmentOf(undeclared) = %v", got)
		}
		if got := ContainmentOf(nil); got != ContainmentNone {
			t.Fatalf("ContainmentOf(nil) = %v", got)
		}
	})
	t.Run("a level outside the scale is the weakest tier", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			n := rapid.OneOf(
				rapid.IntRange(math.MinInt32, int(ContainmentNone)-1),
				rapid.IntRange(int(ContainmentRemote)+1, math.MaxInt32),
			).Draw(rt, "level")
			sb := tier{Containment(n)}
			if got := ContainmentOf(sb); got != ContainmentNone {
				rt.Fatalf("ContainmentOf(level %d) = %v, want %v", n, got, ContainmentNone)
			}
			for _, trust := range []Trust{TrustSemi, TrustUntrusted} {
				if Admit(sb, trust) == nil {
					rt.Fatalf("Admit let %v work into a sandbox declaring level %d", trust, n)
				}
			}
			if _, err := Select(TrustSemi, sb); err == nil {
				rt.Fatalf("Select chose a sandbox declaring level %d for semi-trusted work", n)
			}
		})
	})
}
