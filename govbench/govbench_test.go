package govbench_test

import (
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/govbench"
)

// update regenerates the golden report artifact: go test ./govbench -update.
var update = flag.Bool("update", false, "regenerate the golden ASR report")

// benignFloor is the minimum benign-pass the gate accepts under least-privilege. A
// change that denies legitimate work drops below this and fails the build, so the
// gate cannot be satisfied by refusing everything.
const benignFloor = 1.0

func run() govbench.Report {
	return govbench.Run(govbench.Corpus(), govbench.RegimePolicyText, govbench.RegimeLeastPrivilege)
}

func result(t *testing.T, rep govbench.Report, name string) govbench.RegimeResult {
	t.Helper()
	for _, r := range rep.Results {
		if r.Regime == name {
			return r
		}
	}
	t.Fatalf("regime %q missing from report", name)
	return govbench.RegimeResult{}
}

// TestPerCaseAnchor is the primary regression gate: every case must land on its
// declared least-privilege disposition. If a change to the waist admits an
// out-of-grant attack (WantLeastPriv Blocked -> Admitted) the ASR rose and this
// fails; if it denies legitimate work (benign Admitted -> Blocked) the floor broke
// and this fails. Closing a residual attack is an intentional change: flip that
// case's anchor to Blocked and set ClosedBy in the same commit.
func TestPerCaseAnchor(t *testing.T) {
	lp := result(t, run(), govbench.RegimeLeastPrivilege.Name)
	for _, o := range lp.Outcomes {
		if o.Got != o.Case.WantLeastPriv {
			t.Errorf("case %s: least-privilege disposition = %s, want %s (%s)",
				o.Case.ID, o.Got, o.Case.WantLeastPriv, o.Case.Note)
		}
	}
}

// TestBenignFloor holds the benign-pass floor under least-privilege: all
// legitimate work is admitted.
func TestBenignFloor(t *testing.T) {
	lp := result(t, run(), govbench.RegimeLeastPrivilege.Name)
	if lp.BenignPass < benignFloor {
		t.Errorf("benign-pass under least-privilege = %.3f, want >= %.3f (%d/%d admitted)",
			lp.BenignPass, benignFloor, lp.BenignAdmitted, lp.BenignTotal)
	}
}

// TestASRMatchesAnchors ties the top-line ASR to the per-case anchors, so the
// number cannot drift without a case moving. The expected residual is exactly the
// attacks whose anchor is Admitted (intent-level abuse of a granted verb).
func TestASRMatchesAnchors(t *testing.T) {
	lp := result(t, run(), govbench.RegimeLeastPrivilege.Name)

	var attacks, residual int
	for _, c := range govbench.Corpus() {
		if !c.Attack {
			continue
		}
		attacks++
		if c.WantLeastPriv == govbench.Admitted {
			residual++
		}
	}
	wantASR := float64(residual) / float64(attacks)
	if !approx(lp.ASR, wantASR) {
		t.Errorf("least-privilege ASR = %.4f, want %.4f (%d residual / %d attacks)",
			lp.ASR, wantASR, residual, attacks)
	}
	if lp.AttacksAdmitted != residual {
		t.Errorf("least-privilege admitted %d attacks, want %d (the tracked residual)",
			lp.AttacksAdmitted, residual)
	}
}

// TestMechanicalContribution is the ablation: least-privilege must lower ASR
// against the policy-text baseline, and that baseline must admit every attack (no
// mechanical enforcement means no denial). This proves the mechanical layer's
// contribution instead of asserting it.
func TestMechanicalContribution(t *testing.T) {
	rep := run()
	base := result(t, rep, govbench.RegimePolicyText.Name)
	lp := result(t, rep, govbench.RegimeLeastPrivilege.Name)

	if !approx(base.ASR, 1.0) {
		t.Errorf("policy-text ASR = %.3f, want 1.000 (no grant means nothing is denied)", base.ASR)
	}
	if !approx(base.BenignPass, 1.0) {
		t.Errorf("policy-text benign-pass = %.3f, want 1.000", base.BenignPass)
	}
	if lp.ASR >= base.ASR {
		t.Errorf("least-privilege ASR %.3f did not improve on policy-text %.3f: mechanical layer added nothing",
			lp.ASR, base.ASR)
	}
	if got := rep.MechanicalContribution(); got <= 0 {
		t.Errorf("mechanical contribution = %.3f, want > 0", got)
	}
}

// TestResidualIsTracked keeps the residual honest: every attack that survives
// least-privilege must name the layer that closes it, so a surviving attack is
// always tied to tracked work and never silently accepted.
func TestResidualIsTracked(t *testing.T) {
	lp := result(t, run(), govbench.RegimeLeastPrivilege.Name)
	for _, o := range lp.Outcomes {
		if o.Case.Attack && o.Got == govbench.Admitted && o.Case.ClosedBy == "" {
			t.Errorf("residual attack %s survives least-privilege but names no closing layer (ClosedBy empty)", o.Case.ID)
		}
	}
}

// TestGoldenReport pins the publishable report artifact. Regenerate with -update
// when the numbers change intentionally; a stray change to the corpus or the waist
// shows up as a diff here for review.
func TestGoldenReport(t *testing.T) {
	got := run().String()
	golden := filepath.Join("testdata", "asr_report.txt")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("report drifted from golden; run: go test ./govbench -update\n--- got ---\n%s", got)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
