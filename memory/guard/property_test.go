package guard_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// untrustedSchemes are the provenance prefixes that must always classify untrusted.
var untrustedSchemes = []string{guard.SchemeTool, guard.SchemeInbound, guard.SchemeWeb, guard.SchemeExternal}

// TestProp_TrustClassification: an untrusted scheme prefix always yields untrusted;
// the user scheme always yields trusted; anything else is semi and is never
// silently promoted to trusted. Trust is a pure function of the source string.
func TestProp_TrustClassification(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		rest := rapid.String().Draw(rt, "rest")

		scheme := rapid.SampledFrom(untrustedSchemes).Draw(rt, "untrusted")
		if got := guard.TrustOf(scheme + rest); got != sandbox.TrustUntrusted {
			rt.Fatalf("TrustOf(%q) = %v, want untrusted", scheme+rest, got)
		}
		if got := guard.TrustOf(guard.SchemeUser + rest); got != sandbox.TrustTrusted {
			rt.Fatalf("TrustOf(user:%q) = %v, want trusted", rest, got)
		}
		// A bare token with no known scheme is never trusted.
		bare := rapid.StringMatching(`[a-z0-9-]{0,12}`).Draw(rt, "bare")
		if got := guard.TrustOf(bare); got == sandbox.TrustTrusted {
			rt.Fatalf("bare source %q classified trusted; must be at most semi", bare)
		}
	})
}

// TestProp_ScreenDeterministic: Screen is a pure function, so repeated calls agree.
func TestProp_ScreenDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "content")
		a, b := guard.Screen(s), guard.Screen(s)
		if len(a) != len(b) {
			rt.Fatalf("Screen not deterministic: %d vs %d findings", len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				rt.Fatalf("finding %d differs: %v vs %v", i, a[i], b[i])
			}
		}
	})
}

// TestProp_ZeroWidthAlwaysCaught: injecting a zero-width space into any content
// always produces at least one finding, and a structural finding sorts first. This
// is the deterministic wall the package leans on.
func TestProp_ZeroWidthAlwaysCaught(t *testing.T) {
	const zwsp = "\u200b"
	rapid.Check(t, func(rt *rapid.T) {
		base := rapid.StringMatching(`[a-zA-Z0-9 ]{0,20}`).Draw(rt, "base")
		at := rapid.IntRange(0, len(base)).Draw(rt, "at")
		poisoned := base[:at] + zwsp + base[at:]

		findings := guard.Screen(poisoned)
		if len(findings) == 0 {
			rt.Fatalf("zero-width space not caught in %q", poisoned)
		}
		if !findings[0].Structural() {
			rt.Fatalf("first finding %v not structural", findings[0])
		}
	})
}

// TestProp_StoreRefusesIffUntrustedHit: the decorator's refusal is exactly
// "untrusted origin AND a screening hit". Trusted/semi origins always pass, and a
// clean untrusted write always passes. The refusal, when it fires, is Forbidden.
func TestProp_StoreRefusesIffUntrustedHit(t *testing.T) {
	const zwsp = "\u200b"
	rapid.Check(t, func(rt *rapid.T) {
		g := guard.Wrap(state.NewMemory().Memory())

		base := rapid.StringMatching(`[a-zA-Z0-9 ]{1,20}`).Draw(rt, "base")
		poison := rapid.Bool().Draw(rt, "poison")
		content := base
		if poison {
			content = base + zwsp
		}
		source := rapid.SampledFrom([]string{
			guard.SchemeTool + "x", guard.SchemeWeb + "y", // untrusted
			guard.SchemeUser + "op", "run-1", "chat", "", // trusted / semi
		}).Draw(rt, "source")

		_, err := g.Write(context.Background(), state.MemoryItem{Content: content, Source: source})

		wantRefuse := guard.TrustOf(source) == sandbox.TrustUntrusted && len(guard.Screen(content)) > 0
		if wantRefuse {
			if err == nil {
				rt.Fatalf("expected refusal for untrusted poisoned write (source %q)", source)
			}
			if fault.Classify(err) != fault.Forbidden {
				rt.Fatalf("refusal class = %v, want Forbidden", fault.Classify(err))
			}
		} else if err != nil {
			rt.Fatalf("unexpected refusal (source %q, poison %v): %v", source, poison, err)
		}
	})
}
