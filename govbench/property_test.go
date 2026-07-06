package govbench_test

import (
	"strconv"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/govbench"
)

// nameGen draws a plausible action name.
var nameGen = rapid.StringMatching(`[a-z][a-z0-9_.]{0,8}`)

// synthCorpus builds a corpus from a drawn set of benign names and attack names.
// The grant under least-privilege is exactly the benign names, so an attack whose
// name is not among them is out-of-grant, and one that reuses a benign name is
// in-grant misuse.
func synthCorpus(rt *rapid.T) []govbench.Case {
	benign := rapid.SliceOfDistinct(nameGen, func(s string) string { return s }).Draw(rt, "benign")
	attacks := rapid.SliceOf(nameGen).Draw(rt, "attacks")

	benignSet := map[string]bool{}
	corpus := make([]govbench.Case, 0, len(benign)+len(attacks))
	for i, n := range benign {
		benignSet[n] = true
		corpus = append(corpus, govbench.Case{
			ID: "benign-" + strconv.Itoa(i), Attack: false,
			Action: dispatch.Action{Name: n}, WantLeastPriv: govbench.Admitted,
		})
	}
	for i, n := range attacks {
		want := govbench.Blocked
		closedBy := ""
		if benignSet[n] {
			want = govbench.Admitted // in-grant misuse survives the name-gate
			closedBy = "higher-layer"
		}
		corpus = append(corpus, govbench.Case{
			ID: "attack-" + strconv.Itoa(i), Attack: true,
			Action: dispatch.Action{Name: n}, WantLeastPriv: want, ClosedBy: closedBy,
		})
	}
	return corpus
}

// Property: mechanical least-privilege enforcement never increases attack success
// over policy-text, always holds the benign floor, and its per-case dispositions
// match ground truth (out-of-grant blocked, benign and in-grant-misuse admitted).
// This is measured against the real dispatch waist, so it is a property of the
// production gate, not of a model.
func TestProp_LeastPrivilegeNeverWorseThanPolicyText(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		corpus := synthCorpus(rt)
		rep := govbench.Run(corpus, govbench.RegimePolicyText, govbench.RegimeLeastPrivilege)

		var base, lp govbench.RegimeResult
		for _, r := range rep.Results {
			switch r.Regime {
			case govbench.RegimePolicyText.Name:
				base = r
			case govbench.RegimeLeastPrivilege.Name:
				lp = r
			}
		}

		// Policy-text binds no grant, so it admits every action: ASR is 1.0 whenever
		// there is at least one attack, and benign-pass is always 1.0.
		if base.AttacksTotal > 0 && base.ASR != 1.0 {
			rt.Fatalf("policy-text ASR = %v with %d attacks, want 1.0", base.ASR, base.AttacksTotal)
		}
		if base.BenignTotal > 0 && base.BenignPass != 1.0 {
			rt.Fatalf("policy-text benign-pass = %v, want 1.0", base.BenignPass)
		}
		// Mechanical enforcement never raises ASR and never lowers the benign floor.
		if lp.ASR > base.ASR {
			rt.Fatalf("least-privilege ASR %v exceeds policy-text %v", lp.ASR, base.ASR)
		}
		if lp.BenignTotal > 0 && lp.BenignPass != 1.0 {
			rt.Fatalf("least-privilege benign-pass = %v, want 1.0 (benign names are exactly the grant)", lp.BenignPass)
		}
		// Every case lands on its ground-truth disposition under least-privilege.
		for _, o := range lp.Outcomes {
			if o.Got != o.Case.WantLeastPriv {
				rt.Fatalf("case %s: got %s, want %s", o.Case.ID, o.Got, o.Case.WantLeastPriv)
			}
		}
	})
}
