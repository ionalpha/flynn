package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

func term(id, check string) goal.Invariant {
	return goal.Invariant{ID: id, Statement: "never force-push a shared branch", Check: check}
}

// TestAuditRulesOnTheExitCode: the verdict is what the check did, not what anyone says
// about it. A zero exit holds; anything else is a breach carrying the output as what was
// observed, because a stopped goal that says only "a term was broken" is unactionable.
func TestAuditRulesOnTheExitCode(t *testing.T) {
	tests := []struct {
		name       string
		exit       int
		wantBreach bool
	}{
		{name: "holds", exit: 0},
		{name: "broken", exit: 1, wantBreach: true},
		{name: "broken with an unusual code", exit: 137, wantBreach: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{exit: tc.exit, output: "pushed --force to main"}
			a := NewCommandAuditor(sb, spine.NewMemoryLog(), nil)

			breaches, err := a.Audit(context.Background(), goalRes("run-1"), goal.Spec{}, goal.Status{},
				[]goal.Invariant{term("no-force-push", "git reflog | grep -q forced")})
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			if !tc.wantBreach {
				if len(breaches) != 0 {
					t.Fatalf("a check that exited 0 reported %+v", breaches)
				}
				return
			}
			if len(breaches) != 1 || breaches[0].ID != "no-force-push" {
				t.Fatalf("breaches = %+v, want one against no-force-push", breaches)
			}
			if !strings.Contains(breaches[0].Detail, "pushed --force to main") {
				t.Fatalf("the breach does not say what was observed: %q", breaches[0].Detail)
			}
			if got := sb.lines; len(got) != 1 || got[0] != "git reflog | grep -q forced" {
				t.Fatalf("the sandbox ran %v, want the term's declared check", got)
			}
		})
	}
}

// TestEveryAuditIsRecorded: the audit is on the run's own log, for the term that held as
// well as the one that did not. A log that only ever mentions breaches cannot show that
// the terms were checked, which is the thing worth being able to show afterwards.
func TestEveryAuditIsRecorded(t *testing.T) {
	log := spine.NewMemoryLog()
	sb := &fakeSandbox{exit: 0, output: "clean"}
	ctx := context.Background()
	a := NewCommandAuditor(sb, log, nil)

	if _, err := a.Audit(ctx, goalRes("run-2"), goal.Spec{}, goal.Status{}, []goal.Invariant{
		term("a", "true"),
		term("b", "true"),
	}); err != nil {
		t.Fatalf("Audit: %v", err)
	}

	events, err := log.Read(ctx, spine.Query{Stream: "run-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("recorded %d audits, want one per term", len(events))
	}
	e := events[0]
	if e.Type != chain.InvariantAudited {
		t.Fatalf("event type = %q, want %q", e.Type, chain.InvariantAudited)
	}
	if e.Actor != spine.ActorSystem {
		t.Fatalf("actor = %q, want the runtime rather than the agent", e.Actor)
	}
	if e.Payload[chain.InvariantKey] != "a" {
		t.Fatalf("the audit does not name its term: %+v", e.Payload)
	}
	if e.Payload[chain.InvariantHeldKey] != true {
		t.Fatalf("a check that exited 0 was not recorded as held: %+v", e.Payload)
	}
	if e.Payload[chain.ItemProvenanceKey] != chain.ProvenanceExecuted {
		t.Fatalf("provenance = %v, want executed: the check was run", e.Payload[chain.ItemProvenanceKey])
	}
	if got := e.Payload[chain.ItemOutputKey]; got != outputHash("clean") {
		t.Fatalf("output hash = %v, want the hash of what the check printed", got)
	}
	if _, ok := e.Payload[chain.InvariantDetailKey]; ok {
		t.Fatalf("a term that held recorded a breach detail: %+v", e.Payload)
	}
}

// TestABreachIsRecordedWithWhatWasObserved: the breach on the record carries the same
// detail the goal stalls on, so the log stands on its own.
func TestABreachIsRecordedWithWhatWasObserved(t *testing.T) {
	log := spine.NewMemoryLog()
	ctx := context.Background()
	a := NewCommandAuditor(&fakeSandbox{exit: 3, output: "two secrets in the diff"}, log, nil)

	breaches, err := a.Audit(ctx, goalRes("run-3"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{term("no-secrets", "scan-diff")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	events, err := log.Read(ctx, spine.Query{Stream: "run-3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Payload[chain.InvariantHeldKey] != false {
		t.Fatalf("a check that exited 3 was recorded as holding: %+v", events[0].Payload)
	}
	detail, _ := events[0].Payload[chain.InvariantDetailKey].(string)
	if detail != breaches[0].Detail {
		t.Fatalf("the record says %q and the goal stalls on %q", detail, breaches[0].Detail)
	}
	if !strings.Contains(detail, "two secrets in the diff") {
		t.Fatalf("the recorded detail does not say what was observed: %q", detail)
	}
}

// TestAnAuditThatCannotRunIsAnError is the fail-closed rule, and it is the one place this
// differs from the item verifier on purpose. An item that cannot be checked is unproven
// and the run carries on trying; a term that cannot be checked means the run is no longer
// governed, so every way of not getting an answer is an error the reconciler stops on
// rather than a clean finding of no breach.
func TestAnAuditThatCannotRunIsAnError(t *testing.T) {
	tests := []struct {
		name  string
		a     *CommandAuditor
		terms []goal.Invariant
		class fault.Class
		code  string
	}{
		{
			name:  "no check declared",
			a:     NewCommandAuditor(&fakeSandbox{}, spine.NewMemoryLog(), nil),
			terms: []goal.Invariant{{ID: "prose-only", Statement: "be careful"}},
			class: fault.Terminal,
			code:  "audit_no_check",
		},
		{
			name:  "a check of only whitespace",
			a:     NewCommandAuditor(&fakeSandbox{}, spine.NewMemoryLog(), nil),
			terms: []goal.Invariant{term("blank", "   \n ")},
			class: fault.Terminal,
			code:  "audit_no_check",
		},
		{
			name:  "no sandbox to run it in",
			a:     NewCommandAuditor(nil, spine.NewMemoryLog(), nil),
			terms: []goal.Invariant{term("a", "true")},
			class: fault.Terminal,
			code:  "audit_no_sandbox",
		},
		{
			name:  "the check could not be started",
			a:     NewCommandAuditor(&fakeSandbox{execErr: errors.New("no shell")}, spine.NewMemoryLog(), nil),
			terms: []goal.Invariant{term("a", "true")},
			class: fault.Terminal,
			code:  "audit_check_unrun",
		},
		{
			name: "the audit was not admitted",
			a: NewCommandAuditor(&fakeSandbox{}, spine.NewMemoryLog(),
				nil, dispatch.WithAdmitter(refusingAdmitter{})),
			terms: []goal.Invariant{term("a", "true")},
			class: fault.Forbidden,
			code:  "audit_check_unrun",
		},
		{
			name:  "no log to record the audit on",
			a:     NewCommandAuditor(&fakeSandbox{}, nil, nil),
			terms: []goal.Invariant{term("a", "true")},
			class: fault.Terminal,
			code:  "audit_no_log",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			breaches, err := tc.a.Audit(context.Background(), goalRes("run-4"), goal.Spec{}, goal.Status{}, tc.terms)
			if err == nil {
				t.Fatalf("an audit that could not run returned %+v and no error", breaches)
			}
			if breaches != nil {
				t.Fatalf("an audit that could not run also reported findings: %+v", breaches)
			}
			if got := fault.Classify(err); got != tc.class {
				t.Fatalf("classified %q, want %q", got, tc.class)
			}
			var fe *fault.Error
			if !errors.As(err, &fe) || fe.Code != tc.code {
				t.Fatalf("fault code %v, want %q", err, tc.code)
			}
		})
	}
}

// TestAnAppendFailureFailsTheAudit: the record is not optional. An audit nobody can show
// happened is what auditing on the spine exists to prevent, so a log that refuses the
// write fails the audit rather than being dropped in favour of the verdict.
func TestAnAppendFailureFailsTheAudit(t *testing.T) {
	a := NewCommandAuditor(&fakeSandbox{exit: 0}, failingLog{}, nil)

	_, err := a.Audit(context.Background(), goalRes("run-5"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{term("a", "true")})
	if err == nil {
		t.Fatal("an audit that could not be recorded was reported as a clean audit")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("classified %q, want %q so the write is retried", got, fault.Transient)
	}
}

// TestTheAuditRunsUnderTheGoalsOwnGrant: the reconcile path carries no run's authority,
// so the audit binds the goal's. A goal whose grant does not carry the audit action
// cannot be audited by running its checks, and it fails closed rather than being waved
// through.
func TestTheAuditRunsUnderTheGoalsOwnGrant(t *testing.T) {
	audited := NewCommandAuditor(&fakeSandbox{exit: 0}, spine.NewMemoryLog(),
		nil, dispatch.WithAdmitter(capability.Admitter{}))
	ctx := context.Background()
	terms := []goal.Invariant{term("a", "true")}

	granted := goal.Spec{Grant: []string{AuditInvariantAction}}
	if _, err := audited.Audit(ctx, goalRes("run-6"), granted, goal.Status{}, terms); err != nil {
		t.Fatalf("a goal granted the audit action could not be audited: %v", err)
	}

	withheld := goal.Spec{Grant: []string{"shell"}}
	_, err := audited.Audit(ctx, goalRes("run-6"), withheld, goal.Status{}, terms)
	if err == nil {
		t.Fatal("the audit ran under a grant that does not carry it")
	}
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("classified %q, want %q", got, fault.Forbidden)
	}

	// A goal with no grant is unconstrained, exactly as it is everywhere else, so an
	// empty grant is not bound as one that allows nothing.
	if _, err := audited.Audit(ctx, goalRes("run-6"), goal.Spec{}, goal.Status{}, terms); err != nil {
		t.Fatalf("a goal with no grant could not be audited: %v", err)
	}
}

// TestOneBrokenTermDoesNotHideTheRest: the terms are audited in order and every finding
// is returned, so a run that broke two of them is not reported as having broken one.
func TestOneBrokenTermDoesNotHideTheRest(t *testing.T) {
	sb := &perCommandSandbox{exits: map[string]int{"check-a": 1, "check-b": 0, "check-c": 2}}
	a := NewCommandAuditor(sb, spine.NewMemoryLog(), nil)

	breaches, err := a.Audit(context.Background(), goalRes("run-7"), goal.Spec{}, goal.Status{},
		[]goal.Invariant{term("a", "check-a"), term("b", "check-b"), term("c", "check-c")})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(breaches) != 2 || breaches[0].ID != "a" || breaches[1].ID != "c" {
		t.Fatalf("breaches = %+v, want a and c", breaches)
	}
}

// perCommandSandbox exits differently per command line, so one audit pass can hold for
// one term and fail for another.
type perCommandSandbox struct {
	fakeSandbox
	exits map[string]int
}

func (p *perCommandSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	p.lines = append(p.lines, cmd.Line)
	return sandbox.ExecResult{ExitCode: p.exits[cmd.Line], Output: cmd.Line + " ran"}, nil
}

// failingLog refuses every append, standing in for a record that cannot be written.
type failingLog struct{ spine.Log }

func (failingLog) Append(context.Context, spine.AppendInput) (spine.Event, error) {
	return spine.Event{}, errors.New("the log is unavailable")
}

// TestACancelledAuditIsTheCancellation: shutting the run down is not a finding about its
// terms, so a cancelled context surfaces as the cancellation rather than as an audit
// that could not run.
func TestACancelledAuditIsTheCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := NewCommandAuditor(&fakeSandbox{execErr: errors.New("context cancelled")}, spine.NewMemoryLog(), nil)

	_, err := a.Audit(ctx, goalRes("run-8"), goal.Spec{}, goal.Status{}, []goal.Invariant{term("a", "true")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
}
