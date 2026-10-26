package governed

import (
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Property: however the operator spells the list of gated actions, the policy holds one
// signature for each distinct non-blank trimmed name and nothing else, and the announced
// action list is that same set in sorted order. Repeating a name never raises its quorum,
// and a blank never becomes a requirement nothing could satisfy.
func TestProp_ApprovalPolicyIsTheDistinctNonBlankSet(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SampledFrom([]string{"shell", "write", " write ", "net.fetch", "", "  ", "deploy"})
		in := rapid.SliceOfN(names, 0, 8).Draw(rt, "actions")

		want := map[string]bool{}
		for _, n := range in {
			if n = strings.TrimSpace(n); n != "" {
				want[n] = true
			}
		}
		policy := ApprovalPolicy(in)
		if len(policy) != len(want) {
			rt.Fatalf("policy has %d requirements for %d distinct names (%v)", len(policy), len(want), in)
		}
		for n := range want {
			if policy[n] != 1 {
				rt.Fatalf("%q requires %d signatures, want 1", n, policy[n])
			}
		}
		gated := GatedActions(policy)
		if !sort.StringsAreSorted(gated) || len(gated) != len(want) {
			rt.Fatalf("gated actions = %v for %v", gated, in)
		}
	})
}

// Property: waiving any subset of the waivable guards with a reason is reflected in the
// run's posture exactly, and the spine holds one event per waiver. A guard that was not
// waived is never reported as waived, and one that was is never reported as wired.
func TestProp_PostureReportsExactlyTheWaivedGuards(t *testing.T) {
	dir := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) {
		var opts []Option
		want := map[Guard]bool{}
		for _, g := range []Guard{GuardApproval, GuardBrakes, GuardBudget, GuardAudit} {
			if rapid.Bool().Draw(rt, "waive "+string(g)) {
				opts = append(opts, Waive(g, "reason for "+string(g)))
				want[g] = true
			}
		}
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), say("done"), b, append([]Option{WithWorkDir(dir)}, opts...)...)
		if err != nil {
			rt.Fatalf("assemble: %v", err)
		}
		defer func() { _ = run.Close() }()

		p := run.Posture()
		if len(p.Waived) != len(want) {
			rt.Fatalf("posture waived %v, asked for %v", p.Waived, want)
		}
		for g := range want {
			if _, ok := p.Waived[g]; !ok {
				rt.Fatalf("%s waived but not reported", g)
			}
		}
		if p.Approval == want[GuardApproval] || p.Brakes == want[GuardBrakes] || p.Budget == want[GuardBudget] {
			rt.Fatalf("a waived guard is reported wired, or the reverse: %+v", p)
		}
		if p.Auditor == want[GuardAudit] {
			rt.Fatalf("audit waiver and auditor disagree: %+v", p)
		}
		if got := len(waivedEvents(t, b.Log, run.ID())); got != len(want) {
			rt.Fatalf("spine holds %d waivers, want %d", got, len(want))
		}
	})
}
