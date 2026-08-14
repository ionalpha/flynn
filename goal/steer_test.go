package goal

import (
	"errors"
	"strings"
	"testing"
)

func wrongTable() Steer {
	return Steer{ID: "use-events-table", Instruction: "you are writing to sessions; write to events instead"}
}

// --- what a redirect has to say to be one -------------------------------------

func TestValidateSteersRefusesOneNobodyCouldDeliver(t *testing.T) {
	cases := []struct {
		name   string
		steers []Steer
		want   error
	}{
		{"no id", []Steer{{Instruction: "use the events table"}}, ErrSteerIncomplete},
		{"no instruction", []Steer{{ID: "s1"}}, ErrSteerIncomplete},
		{"blank instruction", []Steer{{ID: "s1", Instruction: "   \n"}}, ErrSteerIncomplete},
		{"same id twice", []Steer{wrongTable(), {ID: "use-events-table", Instruction: "something else"}}, ErrSteerDuplicate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateSteers(c.steers); !errors.Is(err, c.want) {
				t.Fatalf("ValidateSteers = %v, want %v", err, c.want)
			}
		})
	}
	if err := ValidateSteers([]Steer{wrongTable(), {ID: "s2", Instruction: "stop touching the migration"}}); err != nil {
		t.Fatalf("two well-formed redirects were refused: %v", err)
	}
}

// --- the no-withdrawal rule ----------------------------------------------------

// TestARedirectCannotBeWithdrawnOrReworded is the rule that separates an obligation from
// a suggestion. The party under it must not be able to edit it, and the spec is not a
// surface only the operator can reach.
func TestARedirectCannotBeWithdrawnOrReworded(t *testing.T) {
	var st Status
	st.SyncSteers([]Steer{wrongTable()})

	if err := st.ValidateSteersGiven(nil); !errors.Is(err, ErrSteerWithdrawn) {
		t.Fatalf("dropping a redirect the run was given = %v, want ErrSteerWithdrawn", err)
	}
	softened := []Steer{{ID: "use-events-table", Instruction: "consider writing to events where convenient"}}
	if err := st.ValidateSteersGiven(softened); !errors.Is(err, ErrSteerWithdrawn) {
		t.Fatalf("rewording a redirect = %v, want ErrSteerWithdrawn", err)
	}
	// Issuing another one is always allowed: it is how an operator corrects one that was
	// wrong, and there is no version of "the run got out of an obligation" that involves
	// acquiring a second.
	more := []Steer{wrongTable(), {ID: "s2", Instruction: "leave the migration alone"}}
	if err := st.ValidateSteersGiven(more); err != nil {
		t.Fatalf("issuing a further redirect was refused: %v", err)
	}
}

// TestADischargedRedirectIsStillProtected: deleting an acknowledged redirect would erase
// the account that discharged it, which is the only durable evidence the operator was
// answered.
func TestADischargedRedirectIsStillProtected(t *testing.T) {
	var st Status
	st.SyncSteers([]Steer{wrongTable()})
	st.RecordAcknowledgements([]Steer{wrongTable()}, []Acknowledgement{{ID: "use-events-table", How: "rewrote the writer to target events"}}, testNow)

	if err := st.ValidateSteersGiven(nil); !errors.Is(err, ErrSteerWithdrawn) {
		t.Fatalf("an acknowledged redirect was deletable: %v", err)
	}
}

// --- taking one on --------------------------------------------------------------

func TestSyncSteersTakesOneOnOnceAndKeepsWhatIsRecorded(t *testing.T) {
	var st Status
	st.SyncSteers([]Steer{wrongTable()})
	st.RecordAcknowledgements([]Steer{wrongTable()}, []Acknowledgement{{ID: "use-events-table", How: "rewrote the writer"}}, testNow)
	st.SyncSteers([]Steer{wrongTable()})

	if len(st.Steers) != 1 {
		t.Fatalf("a redirect was taken on twice: %+v", st.Steers)
	}
	if !st.Steers[0].Acknowledged || st.Steers[0].Account != "rewrote the writer" {
		t.Fatalf("re-syncing cleared the account: %+v", st.Steers[0])
	}
	if st.Steers[0].Mark != SteerMark(wrongTable()) {
		t.Fatalf("mark = %q, want the content fingerprint", st.Steers[0].Mark)
	}
}

// TestANewRedirectClearsTheNonConvergenceCount: that count stops a run that is being told
// nothing new, and an operator redirect is the run being told something new by the party
// the count was standing in for.
func TestANewRedirectClearsTheNonConvergenceCount(t *testing.T) {
	st := Status{VerdictMark: "m", VerdictRepeat: VerdictRepeatLimit, LastVerdict: "still not satisfied"}
	if !st.StalledForNonConvergence() {
		t.Fatal("the fixture is not at the non-convergence limit")
	}
	st.SyncSteers([]Steer{wrongTable()})
	if st.StalledForNonConvergence() {
		t.Fatalf("a redirected run was still stalled for non-convergence: %+v", st)
	}
	// A pass that takes on nothing new leaves the count alone, so an ordinary reconcile
	// cannot reset it by re-syncing the same redirect.
	st.ObserveVerdict("still not satisfied", "")
	st.ObserveVerdict("still not satisfied", "")
	st.SyncSteers([]Steer{wrongTable()})
	if !st.StalledForNonConvergence() {
		t.Fatalf("re-syncing an old redirect cleared the count: %+v", st)
	}
}

// --- what counts as outstanding ---------------------------------------------------

func TestOutstandingSteers(t *testing.T) {
	second := Steer{ID: "s2", Instruction: "leave the migration alone"}
	var st Status
	st.SyncSteers([]Steer{wrongTable()})
	st.RecordAcknowledgements([]Steer{wrongTable()}, []Acknowledgement{{ID: "use-events-table", How: "done"}}, testNow)

	// The second redirect has no status entry yet. Being taken on is what the run records
	// about a redirect, never what discharges it, so one the status has not caught up with
	// is one nobody has answered.
	open := st.OutstandingSteers([]Steer{wrongTable(), second})
	if len(open) != 1 || open[0].ID != "s2" {
		t.Fatalf("outstanding = %+v, want only the unanswered redirect", open)
	}
	if open := st.OutstandingSteers([]Steer{wrongTable()}); len(open) != 0 {
		t.Fatalf("an acknowledged redirect was still outstanding: %+v", open)
	}
	if open := st.OutstandingSteers(nil); len(open) != 0 {
		t.Fatalf("a run under no redirects had %d outstanding", len(open))
	}
}

// --- recording an acknowledgement --------------------------------------------------

// TestRecordAcknowledgementsDischargesOnlyWhatWasNamed: silence is a refusal, so a judge
// that answers half the question leaves the other half unpaid.
func TestRecordAcknowledgementsDischargesOnlyWhatWasNamed(t *testing.T) {
	second := Steer{ID: "s2", Instruction: "leave the migration alone"}
	outstanding := []Steer{wrongTable(), second}
	var st Status
	st.SyncSteers(outstanding)

	open := st.RecordAcknowledgements(outstanding, []Acknowledgement{{ID: "s2", How: "reverted the migration edit"}}, testNow)
	if len(open) != 1 || open[0].ID != "use-events-table" {
		t.Fatalf("still open = %+v, want the redirect the judge did not name", open)
	}
	if len(st.OutstandingSteers(outstanding)) != 1 {
		t.Fatalf("the status disagrees with the returned set: %+v", st.Steers)
	}
	if open := st.RecordAcknowledgements(outstanding[:1], nil, testNow); len(open) != 1 {
		t.Fatalf("a judge that named nothing discharged something: %+v", open)
	}
}

// TestAFindingAboutSomethingElseIsIgnored: the judge is handed the outstanding redirects
// and rules on those. A finding about anything else would discharge an obligation the
// judge was never asked about, including one already discharged, whose account it would
// overwrite.
func TestAFindingAboutSomethingElseIsIgnored(t *testing.T) {
	settled := Steer{ID: "s2", Instruction: "leave the migration alone"}
	all := []Steer{wrongTable(), settled}
	now := testNow
	var st Status
	st.SyncSteers(all)
	st.RecordAcknowledgements([]Steer{settled}, []Acknowledgement{{ID: "s2", How: "reverted the migration edit"}}, now)

	outstanding := st.OutstandingSteers(all)
	open := st.RecordAcknowledgements(outstanding, []Acknowledgement{
		{ID: "s2", How: "and again, differently"},
		{ID: "never-issued", How: "addressed a redirect nobody made"},
	}, now)

	if len(open) != 1 || open[0].ID != "use-events-table" {
		t.Fatalf("still open = %+v, want the one redirect that was outstanding", open)
	}
	for _, given := range st.Steers {
		if given.ID == "s2" && given.Account != "reverted the migration edit" {
			t.Fatalf("a discharged redirect's account was overwritten: %q", given.Account)
		}
	}
	if len(st.Steers) != 2 {
		t.Fatalf("a finding created an entry for a redirect nobody issued: %+v", st.Steers)
	}
}

// TestARedirectNotYetTakenOnStaysOutstanding: a redirect the status has no entry for has
// nowhere durable to record the discharge, so accepting the finding would drop it. It is
// judged again on the pass that has taken it on.
func TestARedirectNotYetTakenOnStaysOutstanding(t *testing.T) {
	fresh := Steer{ID: "s3", Instruction: "stop touching the migration"}
	var st Status
	st.SyncSteers([]Steer{wrongTable()})

	open := st.RecordAcknowledgements([]Steer{fresh},
		[]Acknowledgement{{ID: "s3", How: "reverted the migration edit"}}, testNow)

	if len(open) != 1 || open[0].ID != "s3" {
		t.Fatalf("still open = %+v, want the redirect with nowhere to record it", open)
	}
	if len(st.Steers) != 1 {
		t.Fatalf("the discharge invented an entry: %+v", st.Steers)
	}
}

// --- what the run and the operator are told -------------------------------------------

func TestSteerBriefStatesTheRedirectAndTheDischargeRule(t *testing.T) {
	spec := Spec{Objective: "add the audit trail", StopCondition: "the trail is written", Steers: []Steer{wrongTable()}}
	var st Status
	st.SyncSteers(spec.Steers)

	brief := SteerBrief(spec, st)
	if !strings.Contains(brief, wrongTable().Instruction) {
		t.Fatalf("the brief does not carry the redirect: %q", brief)
	}
	// A run that is told the rule can answer it; a run that is not gets refused for
	// failing a test nobody described.
	if !strings.Contains(brief, "state for each one what you did about it") {
		t.Fatalf("the brief does not state the discharge rule: %q", brief)
	}
	if strings.Contains(brief, "definition of done") != true {
		t.Fatalf("the brief does not say the objective is unchanged: %q", brief)
	}

	st.RecordAcknowledgements(spec.Steers, []Acknowledgement{{ID: "use-events-table", How: "rewrote the writer"}}, testNow)
	if brief := SteerBrief(spec, st); brief != "" {
		t.Fatalf("a discharged redirect was still being delivered: %q", brief)
	}
	if brief := SteerBrief(Spec{Objective: "x"}, Status{}); brief != "" {
		t.Fatalf("a run under no redirects was handed a brief: %q", brief)
	}
}

func TestUnacknowledgedReasonQuotesBothSides(t *testing.T) {
	msg := UnacknowledgedReason([]Steer{wrongTable()}, "wrote the audit trail to the sessions table")
	if !strings.Contains(msg, wrongTable().Instruction) {
		t.Fatalf("the refusal does not say what was asked for: %q", msg)
	}
	if !strings.Contains(msg, "wrote the audit trail to the sessions table") {
		t.Fatalf("the refusal does not say what the run said instead: %q", msg)
	}
	if msg := UnacknowledgedReason([]Steer{wrongTable()}, "  "); strings.Contains(msg, "account") {
		t.Fatalf("a run that said nothing was quoted as having said something: %q", msg)
	}
}
