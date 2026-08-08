package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/spine"
)

// prose is a term with no check: the kind the model auditor exists for.
func prose(id, statement string) goal.Invariant {
	return goal.Invariant{ID: id, Statement: statement}
}

// verdict renders one auditor reply.
func verdict(json string) llm.Response { return llmtest.SayText(json) }

// seededLog is a memory log with a few of the run's events already on it, so the auditor
// has a record to read and cite.
func seededLog(t *testing.T, stream string) spine.Log {
	t.Helper()
	log := spine.NewMemoryLog()
	for _, e := range []spine.AppendInput{
		{Stream: stream, Type: "step.started", Actor: spine.ActorSystem, Payload: map[string]any{"step": 1}},
		{Stream: stream, Type: "tool.called", Actor: spine.ActorAgent, Payload: map[string]any{"name": "edit", "path": "pkg/api/server.go"}},
	} {
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatalf("seeding the record: %v", err)
		}
	}
	return log
}

// TestAProseTermIsRuledOnAgainstTheRecord: the terms that do not reduce to a command are
// still terms, and this is what settles them. The verdict and what it rested on both go on
// the spine, marked asserted, so a reader can tell an audit that ran something from an
// audit that read something.
func TestAProseTermIsRuledOnAgainstTheRecord(t *testing.T) {
	log := seededLog(t, "run-m1")
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"tool.called edit pkg/api/server.go","held":true}`))
	a := NewModelAuditor(m, log)

	breaches, err := a.Audit(context.Background(), goalRes("run-m1"),
		goal.Spec{Objective: "tidy the api package"}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands under the package the objective names")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("a held term reported %+v", breaches)
	}

	// The record the auditor was given is the run's own, and the term and objective
	// come with it: a term is stated against an objective and means nothing without it.
	req := m.Requests()[0]
	asked := req.Messages[0].TextContent()
	for _, want := range []string{"tidy the api package", "every edit lands under", "pkg/api/server.go"} {
		if !strings.Contains(asked, want) {
			t.Fatalf("the auditor was not shown %q:\n%s", want, asked)
		}
	}

	ev := auditEvents(t, log, "run-m1")
	if len(ev) != 1 {
		t.Fatalf("audits recorded = %d, want 1", len(ev))
	}
	if got := ev[0].Payload[chain.ItemProvenanceKey]; got != chain.ProvenanceAsserted {
		t.Fatalf("provenance = %v, want %q: nothing was run for this verdict", got, chain.ProvenanceAsserted)
	}
	if got, _ := ev[0].Payload[chain.InvariantCitedKey].(string); !strings.Contains(got, "pkg/api/server.go") {
		t.Fatalf("the record does not keep what the audit cited: %q", got)
	}
	if got := ev[0].Payload[chain.InvariantHeldKey]; got != true {
		t.Fatalf("held = %v, want true", got)
	}
}

// TestAProseBreachCarriesWhatWasObserved: a stopped goal saying only "a term was broken"
// is unactionable, so the finding carries the auditor's account of it.
func TestAProseBreachCarriesWhatWasObserved(t *testing.T) {
	log := seededLog(t, "run-m2")
	m := llmtest.NewScripted(verdict(
		`{"absence":false,"cited":"tool.called edit pkg/api/server.go","held":false,"detail":"it edited pkg/api, which the objective does not name"}`))
	a := NewModelAuditor(m, log)

	breaches, err := a.Audit(context.Background(), goalRes("run-m2"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands under the package the objective names")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 1 || breaches[0].ID != "in-scope" {
		t.Fatalf("breaches = %+v, want one against in-scope", breaches)
	}
	if !strings.Contains(breaches[0].Detail, "which the objective does not name") {
		t.Fatalf("the breach does not say what was observed: %q", breaches[0].Detail)
	}
	if ev := auditEvents(t, log, "run-m2"); len(ev) != 1 || ev[0].Payload[chain.InvariantHeldKey] != false {
		t.Fatalf("the breach is not on the record: %+v", ev)
	}
}

// TestTheAuditorWillNotRuleOnAnAbsence is the second layer of the negative-space rule.
// Admission refuses an unsearchable term by reading the author's words, and a term that
// gets past that and which the auditor itself reads as an absence claim is refused here
// rather than passed. The two layers do not fail the same way, so waving an absence claim
// through takes wording that defeats a word list and a model that then denies it is one.
func TestTheAuditorWillNotRuleOnAnAbsence(t *testing.T) {
	log := seededLog(t, "run-m3")
	m := llmtest.NewScripted(verdict(`{"absence":true,"cited":"","held":true}`))
	a := NewModelAuditor(m, log)

	breaches, err := a.Audit(context.Background(), goalRes("run-m3"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("clean", "the shipped artefact carries only what the objective asked for")})
	if err == nil {
		t.Fatalf("an absence claim was ruled on from the record and returned %+v", breaches)
	}
	if breaches != nil {
		t.Fatalf("a refused audit also reported findings: %+v", breaches)
	}
	assertFault(t, err, fault.Terminal, "audit_absence_unsearched")
	if ev := auditEvents(t, log, "run-m3"); len(ev) != 0 {
		t.Fatalf("a refused audit was recorded as an audit: %+v", ev)
	}
}

// TestAnUncitedVerdictIsNotAnAudit: "the term holds" resting on nothing is the sentence a
// run under completion pressure produces most easily. It is treated as no audit at all,
// which stops the goal, rather than as the pass it is shaped like.
func TestAnUncitedVerdictIsNotAnAudit(t *testing.T) {
	replies := []string{
		`{"absence":false,"cited":"","held":true}`,
		`{"absence":false,"cited":"  \n ","held":true}`, // whitespace is not a citation
		`{"absence":false,"held":true}`,                 // the key omitted entirely
	}
	for _, reply := range replies {
		log := seededLog(t, "run-m4")
		a := NewModelAuditor(llmtest.NewScripted(verdict(reply)), log)

		_, err := a.Audit(context.Background(), goalRes("run-m4"), goal.Spec{}, goal.Status{},
			[]goal.Invariant{prose("in-scope", "every edit lands under the package the objective names")})
		if err == nil {
			t.Fatal("a verdict citing nothing was accepted as an audit")
		}
		assertFault(t, err, fault.Terminal, "audit_uncited")
	}
}

// TestADeclaredCheckIsNotJudgedFromTheRecord: where the author wrote the search, running
// it is the audit. A model asked whether a grep would have exited zero is guessing at
// something one command away, so the split between the two auditors is by what the term
// declares and this one refuses to stand in.
func TestADeclaredCheckIsNotJudgedFromTheRecord(t *testing.T) {
	log := seededLog(t, "run-m5")
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"the whole record","held":true}`))
	a := NewModelAuditor(m, log)

	_, err := a.Audit(context.Background(), goalRes("run-m5"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{term("no-secrets", "! git grep -q AKIA")})
	assertFault(t, err, fault.Terminal, "audit_check_not_run")
	if m.Calls() != 0 {
		t.Fatalf("the model was asked about a term that declares a check (%d calls)", m.Calls())
	}
}

// TestAnUnparseableReplyIsNotAPass: the zero value of the verdict reads as "the term
// holds", so a reply that does not parse must be an error. An auditor whose parse failure
// means the terms held is worse than no auditor, because the run looks governed.
func TestAnUnparseableReplyIsNotAPass(t *testing.T) {
	for _, reply := range []string{"the term looks fine to me", "{not json at all}"} {
		log := seededLog(t, "run-m6")
		a := NewModelAuditor(llmtest.NewScripted(verdict(reply)), log)

		breaches, err := a.Audit(context.Background(), goalRes("run-m6"), goal.Spec{}, goal.Status{},
			[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
		if err == nil {
			t.Fatalf("reply %q was read as a verdict: %+v", reply, breaches)
		}
		assertFault(t, err, fault.Terminal, "audit_parse")
	}
}

// TestABreachWithNoDetailStillSaysSomething: a model that rules the term broken and says
// nothing about why leaves the goal stopped with an empty message, so the record says what
// happened instead of nothing.
func TestABreachWithNoDetailStillSaysSomething(t *testing.T) {
	log := seededLog(t, "run-m7")
	a := NewModelAuditor(llmtest.NewScripted(verdict(`{"absence":false,"cited":"step.started","held":false}`)), log)

	breaches, err := a.Audit(context.Background(), goalRes("run-m7"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Detail == "" {
		t.Fatalf("breaches = %+v, want one carrying something to read", breaches)
	}
}

// TestEveryTermIsJudgedAgainstOneReading: the record is read once and every term of the
// pass is ruled on against that reading, so two terms of the same goal are never judged
// against different runs.
func TestEveryTermIsJudgedAgainstOneReading(t *testing.T) {
	log := &countingLog{Log: seededLog(t, "run-m8")}
	m := llmtest.NewScripted(
		verdict(`{"absence":false,"cited":"step.started","held":true}`),
		verdict(`{"absence":false,"cited":"tool.called","held":true}`),
	)
	a := NewModelAuditor(m, log)

	if _, err := a.Audit(context.Background(), goalRes("run-m8"), goal.Spec{}, goal.Status{}, []goal.Invariant{
		prose("in-scope", "every edit lands where the objective says"),
		prose("api-stable", "the existing exported names keep their meaning"),
	}); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if log.reads != 1 {
		t.Fatalf("the record was read %d times for one pass, want 1", log.reads)
	}
	if m.Calls() != 2 {
		t.Fatalf("the model was asked %d times, want once per term", m.Calls())
	}
}

// TestAModelThatCannotBeAskedIsNotAPass: an auditor that could not be reached says nothing
// about whether the terms hold, and the class is the model's own so a rate limit retries
// and a bad request stops the goal.
func TestAModelThatCannotBeAskedIsNotAPass(t *testing.T) {
	log := seededLog(t, "run-m9")
	a := NewModelAuditor(&failingModel{err: fault.New(fault.Transient, "rate_limited", "slow down")}, log)

	breaches, err := a.Audit(context.Background(), goalRes("run-m9"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	if err == nil {
		t.Fatalf("an auditor that could not be asked returned %+v and no error", breaches)
	}
	assertFault(t, err, fault.Transient, "audit_model")
}

// TestACancelledProseAuditIsTheCancellation: shutting the run down is not a finding about
// its terms.
func TestACancelledProseAuditIsTheCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := NewModelAuditor(&failingModel{err: errors.New("request cancelled")}, seededLog(t, "run-m10"))

	_, err := a.Audit(ctx, goalRes("run-m10"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
}

// TestAProseAuditNeedsAModelAndARecord: the two wiring gaps fail closed, the same way the
// command auditor's missing sandbox does.
func TestAProseAuditNeedsAModelAndARecord(t *testing.T) {
	terms := []goal.Invariant{prose("in-scope", "every edit lands where the objective says")}

	_, err := NewModelAuditor(nil, spine.NewMemoryLog()).
		Audit(context.Background(), goalRes("run-m11"), goal.Spec{}, goal.Status{}, terms)
	assertFault(t, err, fault.Terminal, "audit_no_model")

	_, err = NewModelAuditor(llmtest.NewScripted(), nil).
		Audit(context.Background(), goalRes("run-m11"), goal.Spec{}, goal.Status{}, terms)
	assertFault(t, err, fault.Terminal, "audit_no_log")
}

// TestTheWindowBoundsWhatOneAuditReads: the record grows across a run while the term
// being ruled on does not, so an audit reads the tail. The framing and the output cap are
// the host's too, since a model with a narrow window cannot be handed either default.
func TestTheWindowBoundsWhatOneAuditReads(t *testing.T) {
	log := seededLog(t, "run-m12") // step.started, then tool.called
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"tool.called","held":true}`))
	a := NewModelAuditor(m, log,
		WithAuditWindow(1),
		WithAuditSystem("rule on the term and cite what you read"),
		WithAuditMaxTokens(64),
	)

	if _, err := a.Audit(context.Background(), goalRes("run-m12"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")}); err != nil {
		t.Fatalf("Audit: %v", err)
	}

	req := m.Requests()[0]
	if req.System != "rule on the term and cite what you read" || req.MaxTokens != 64 {
		t.Fatalf("the framing was not applied: system=%q maxTokens=%d", req.System, req.MaxTokens)
	}
	asked := req.Messages[0].TextContent()
	if strings.Contains(asked, "step.started") {
		t.Fatalf("a window of one read more than the last event:\n%s", asked)
	}
	if !strings.Contains(asked, "tool.called") {
		t.Fatalf("the window dropped the event it should have kept:\n%s", asked)
	}
}

// TestEmptyOverridesLeaveTheDefaults: an option handed nothing is a caller who did not
// mean to configure anything, and it must not blank the framing that keeps the absence
// disclosure and the citation ahead of the verdict.
func TestEmptyOverridesLeaveTheDefaults(t *testing.T) {
	a := NewModelAuditor(llmtest.NewScripted(), spine.NewMemoryLog(),
		WithAuditSystem("  "), WithAuditMaxTokens(0), WithAuditWindow(-1))
	if a.system != defaultAuditSystem || a.maxTokens != 1024 || a.maxEvents != maxAuditEvents {
		t.Fatalf("an empty override changed the defaults: %d tokens, %d events, system %q",
			a.maxTokens, a.maxEvents, a.system)
	}
}

// TestAnEmptyRecordIsSaidPlainly: a run that has recorded nothing must not read as a run
// whose record simply did not mention the term. The auditor is told which it is.
func TestAnEmptyRecordIsSaidPlainly(t *testing.T) {
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"nothing is recorded","held":false,"detail":"the record does not show it"}`))
	a := NewModelAuditor(m, spine.NewMemoryLog())

	breaches, err := a.Audit(context.Background(), goalRes("run-m13"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 1 {
		t.Fatalf("breaches = %+v, want the term unsettled by an empty record", breaches)
	}
	if !strings.Contains(m.Requests()[0].Messages[0].TextContent(), "recorded nothing") {
		t.Fatal("an empty record was shown as no record at all")
	}
}

// TestTheRecordRendersInAStableOrder: two renderings of the same record are the same
// text, so a term is not judged differently on a map iteration. An event carrying no
// payload renders as its header alone rather than as an empty line of nothing.
func TestTheRecordRendersInAStableOrder(t *testing.T) {
	log := spine.NewMemoryLog()
	for _, e := range []spine.AppendInput{
		{Stream: "run-m17", Type: "step.started", Actor: spine.ActorSystem},
		{Stream: "run-m17", Type: "tool.called", Actor: spine.ActorAgent, Payload: map[string]any{
			"name": "edit", "path": "pkg/api/server.go", "bytes": 412,
		}},
	} {
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	a := NewModelAuditor(llmtest.NewScripted(), log)

	first, err := a.readRecord(context.Background(), goalRes("run-m17"))
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	second, err := a.readRecord(context.Background(), goalRes("run-m17"))
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	if first != second {
		t.Fatalf("two readings differ:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(first, "bytes=412 name=edit path=pkg/api/server.go") {
		t.Fatalf("the payload is not rendered in key order:\n%s", first)
	}
	if !strings.Contains(first, "step.started\n") {
		t.Fatalf("an event with no payload did not render on its own:\n%s", first)
	}
}

// TestARecordThatCannotBeReadIsNotAPass: an audit over a record nobody could read has
// ruled on nothing, and it is transient because the store being briefly unavailable is
// the ordinary reason and it is worth retrying.
func TestARecordThatCannotBeReadIsNotAPass(t *testing.T) {
	a := NewModelAuditor(llmtest.NewScripted(), unreadableLog{spine.NewMemoryLog()})

	_, err := a.Audit(context.Background(), goalRes("run-m14"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	assertFault(t, err, fault.Transient, "audit_read")
}

// TestAProseAuditThatCannotBeRecordedFails: the record is not optional here either. An
// audit nobody can show happened is what auditing on the spine exists to prevent, and it
// matters more for this auditor, whose whole verdict is a judgement.
func TestAProseAuditThatCannotBeRecordedFails(t *testing.T) {
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"step.started","held":true}`))
	a := NewModelAuditor(m, failingLog{seededLog(t, "run-m15")})

	_, err := a.Audit(context.Background(), goalRes("run-m15"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("in-scope", "every edit lands where the objective says")})
	assertFault(t, err, fault.Transient, "audit_append")
}

// TestNoTermsAsksNobody: a pass with nothing to rule on costs no model call.
func TestNoTermsAsksNobody(t *testing.T) {
	m := llmtest.NewScripted()
	a := NewModelAuditor(m, spine.NewMemoryLog())

	breaches, err := a.Audit(context.Background(), goalRes("run-m16"), goal.Spec{}, goal.Status{}, nil)
	if err != nil || breaches != nil {
		t.Fatalf("Audit with no terms = %+v, %v", breaches, err)
	}
	if m.Calls() != 0 {
		t.Fatalf("the model was asked about no terms (%d calls)", m.Calls())
	}
}

// --- routing between the two auditors --------------------------------------------

// TestTermsAreRoutedByWhatTheyDeclare: a term with a check is run, a term without one goes
// to the prose auditor, and one pass of a goal carrying both settles both.
func TestTermsAreRoutedByWhatTheyDeclare(t *testing.T) {
	log := seededLog(t, "run-r1")
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"tool.called","held":false,"detail":"it rewrote the public signature"}`))
	sb := &fakeSandbox{exit: 1, output: "forced-update found"}
	a := NewCommandAuditor(sb, log, NewModelAuditor(m, log))

	breaches, err := a.Audit(context.Background(), goalRes("run-r1"), goal.Spec{}, goal.Status{}, []goal.Invariant{
		prose("api-stable", "the existing exported names keep their meaning"),
		term("no-force-push", "git reflog | grep -q forced"),
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 2 {
		t.Fatalf("breaches = %+v, want one from each auditor", breaches)
	}
	if got := sb.lines; len(got) != 1 || got[0] != "git reflog | grep -q forced" {
		t.Fatalf("the sandbox ran %v, want only the term that declared a check", got)
	}
	if m.Calls() != 1 {
		t.Fatalf("the model was asked %d times, want only about the term with no check", m.Calls())
	}
}

// TestAProseTermWithNowhereToGoStopsTheGoal: with no prose auditor wired, a term that
// declares no check is refused rather than skipped. A host may legitimately want the
// strict shape (only terms that reduce to a command), and this is what it looks like.
func TestAProseTermWithNowhereToGoStopsTheGoal(t *testing.T) {
	a := NewCommandAuditor(&fakeSandbox{}, spine.NewMemoryLog(), nil)

	_, err := a.Audit(context.Background(), goalRes("run-r2"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("api-stable", "the existing exported names keep their meaning")})
	assertFault(t, err, fault.Terminal, "audit_no_check")
}

// TestOnlyProseTermsNeedNoSandbox: a host running prose terms alone has nothing to
// execute, so demanding a sandbox it never uses would be a wiring requirement with no
// behaviour behind it.
func TestOnlyProseTermsNeedNoSandbox(t *testing.T) {
	log := seededLog(t, "run-r3")
	m := llmtest.NewScripted(verdict(`{"absence":false,"cited":"step.started","held":true}`))
	a := NewCommandAuditor(nil, log, NewModelAuditor(m, log))

	breaches, err := a.Audit(context.Background(), goalRes("run-r3"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{prose("api-stable", "the existing exported names keep their meaning")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("breaches = %+v, want none", breaches)
	}
}

// --- helpers ----------------------------------------------------------------------

// assertFault checks that err carries the class and code an audit that could not answer
// must carry, since the reconciler routes on both: the class decides retry or stop, and
// the code is what whoever is handed the goal reads.
func assertFault(t *testing.T, err error, class fault.Class, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want fault %q, got no error", code)
	}
	if got := fault.Classify(err); got != class {
		t.Fatalf("classified %q, want %q (%v)", got, class, err)
	}
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != code {
		t.Fatalf("fault %v, want code %q", err, code)
	}
}

// auditEvents returns the invariant audits recorded on a stream.
func auditEvents(t *testing.T, log spine.Log, stream string) []spine.Event {
	t.Helper()
	all, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var out []spine.Event
	for _, e := range all {
		if e.Type == chain.InvariantAudited {
			out = append(out, e)
		}
	}
	return out
}

// countingLog counts how many times the record was read.
type countingLog struct {
	spine.Log
	reads int
}

func (l *countingLog) Read(ctx context.Context, q spine.Query) ([]spine.Event, error) {
	l.reads++
	return l.Log.Read(ctx, q)
}

// unreadableLog refuses every read, standing in for a record the auditor cannot see.
type unreadableLog struct{ spine.Log }

func (unreadableLog) Read(context.Context, spine.Query) ([]spine.Event, error) {
	return nil, errors.New("the log is unavailable")
}

// failingModel stands in for a model that cannot be reached.
type failingModel struct{ err error }

func (m *failingModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, m.err
}
