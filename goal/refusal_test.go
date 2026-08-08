package goal

import (
	"strings"
	"testing"
)

// refusals builds a refusal list from alternating rule/action pairs, so a test reads as
// the sequence of gates the run met rather than as a struct literal per refusal.
func refusals(pairs ...string) []Refusal {
	if len(pairs)%2 != 0 {
		panic("refusals: want rule/action pairs")
	}
	out := make([]Refusal, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Refusal{Rule: pairs[i], Action: pairs[i+1]})
	}
	return out
}

// TestRunRefusedByOneGateManyWaysStops is the shape the whole thing exists for: one gate
// says no, and the run comes back through a different door each time. No single refusal is
// remarkable and the three together are the verdict.
func TestRunRefusedByOneGateManyWaysStops(t *testing.T) {
	v, stop := ReadRefusals(refusals(
		"capability_denied", "write_file",
		"capability_denied", "bash",
		"capability_denied", "mcp.fs.write",
	))
	if !stop {
		t.Fatalf("three actions refused by one gate did not stop the run: %+v", v)
	}
	if !v.Routed {
		t.Errorf("Routed = false, want the substitution shape, not a retry: %+v", v)
	}
	if v.Rule != "capability_denied" {
		t.Errorf("Rule = %q, want the gate that refused", v.Rule)
	}
	if got, want := strings.Join(v.Actions, ","), "write_file,bash,mcp.fs.write"; got != want {
		t.Errorf("Actions = %q, want %q in first-refusal order", got, want)
	}
	// The reason has to be usable by whoever is handed the stopped run, who is deciding
	// between widening the authority and dropping the objective. Neither decision can be
	// made from "blocked", so every route is named.
	reason := v.RefusalReason()
	for _, want := range []string{"capability_denied", "write_file", "bash", "mcp.fs.write"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not name %q", reason, want)
		}
	}
}

// TestTwoRoutesIsNotYetAVerdict is the concession that keeps this honest. Two actions
// refused by one gate is also what an honestly-scoped run looks like when it needs two
// things its grant does not carry, and stopping that run would make the detector a tax on
// narrow grants rather than a guard.
func TestTwoRoutesIsNotYetAVerdict(t *testing.T) {
	if v, stop := ReadRefusals(refusals(
		"capability_denied", "write_file",
		"capability_denied", "net.dial",
	)); stop {
		t.Fatalf("two refused actions stopped the run: %+v", v)
	}
}

// TestSameActionRefusedRepeatedlyStops covers the other half: the run was told no and
// asked again unchanged. One retry is allowed, because a gate's answer can legitimately
// change when a budget window rolls or an approval lands; a third is a run not listening.
func TestSameActionRefusedRepeatedlyStops(t *testing.T) {
	if v, stop := ReadRefusals(refusals(
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
	)); stop {
		t.Fatalf("a second identical refusal stopped the run: %+v", v)
	}
	v, stop := ReadRefusals(refusals(
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
	))
	if !stop {
		t.Fatalf("three identical refusals did not stop the run: %+v", v)
	}
	if v.Routed {
		t.Errorf("Routed = true, want the retry shape: %+v", v)
	}
	if v.Repeated != "model.generate" || v.Repeats != 3 {
		t.Errorf("Repeated/Repeats = %q/%d, want model.generate/3", v.Repeated, v.Repeats)
	}
	if reason := v.RefusalReason(); !strings.Contains(reason, "model.generate") || !strings.Contains(reason, "unchanged") {
		t.Errorf("reason %q does not say what was asked again", reason)
	}
}

// TestRefusalsAreCountedPerRule is why the rule is recorded at all. A run refused once by
// each of three different gates has met three walls; a run refused three times by one gate
// has met one and kept pushing. Keyed on the fault class those two records are identical,
// which is the reason the waist now carries the code.
func TestRefusalsAreCountedPerRule(t *testing.T) {
	if v, stop := ReadRefusals(refusals(
		"capability_denied", "write_file",
		"containment_unavailable", "bash",
		"needs_approval", "net.dial",
	)); stop {
		t.Fatalf("three unrelated gates stopped the run: %+v", v)
	}
}

// TestSubstitutionOutranksRetry pins the ranking. A run that did both is better described
// by the substitution: coming back by another door says something about the run that
// asking twice does not, and the person handed it should read that sentence first.
func TestSubstitutionOutranksRetry(t *testing.T) {
	v, stop := ReadRefusals(refusals(
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
		"budget_exceeded", "model.generate",
		"capability_denied", "write_file",
		"capability_denied", "bash",
		"capability_denied", "mcp.fs.write",
	))
	if !stop {
		t.Fatal("a run that both retried and substituted did not stop")
	}
	if v.Rule != "capability_denied" || !v.Routed {
		t.Errorf("verdict = %+v, want the substitution under capability_denied even though "+
			"budget_exceeded refused more often", v)
	}
}

// TestRefusalNamingNoRuleIsNotCounted keeps an unattributable refusal from inventing a
// gate. Folding every refusal that named no rule into one bucket would produce a rule that
// refused everything, and the run would be stopped by a gate that does not exist.
func TestRefusalNamingNoRuleIsNotCounted(t *testing.T) {
	if v, stop := ReadRefusals(refusals(
		"", "write_file",
		"", "bash",
		"", "net.dial",
		" ", "mcp.fs.write",
	)); stop {
		t.Fatalf("refusals naming no rule stopped the run: %+v", v)
	}
}

// TestNoRefusalsIsNoVerdict is the case every clean run is in.
func TestNoRefusalsIsNoVerdict(t *testing.T) {
	if v, stop := ReadRefusals(nil); stop {
		t.Fatalf("a run with no refusals was stopped: %+v", v)
	}
}

// TestVerdictIsDeterministic pins the tie-break. Two gates with identical histories must
// not yield a different verdict on a different map iteration, or the same record would
// stop the same run two ways.
func TestVerdictIsDeterministic(t *testing.T) {
	record := refusals(
		"zzz_gate", "a", "zzz_gate", "b", "zzz_gate", "c",
		"aaa_gate", "x", "aaa_gate", "y", "aaa_gate", "z",
	)
	for i := range 32 {
		v, stop := ReadRefusals(record)
		if !stop || v.Rule != "aaa_gate" {
			t.Fatalf("run %d: verdict = %+v, stop = %v; want a stable aaa_gate verdict", i, v, stop)
		}
	}
}
