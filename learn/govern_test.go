package learn

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/state"
)

// TestGovernedVerifierRecordsOnSpine confirms a verification flows through the
// dispatch waist: the inner verdict is returned unchanged, and the check is bracketed
// by start/end events on the spine under the stable action name and the run's scope.
func TestGovernedVerifierRecordsOnSpine(t *testing.T) {
	sink := &dispatch.MemorySink{}
	inner := fakeVerifier{v: Verdict{Verified: true, Ran: true, Detail: "exit 0"}}
	gv := NewGovernedVerifier(inner, dispatch.WithEventSink(sink))
	scope := state.Scope{Instance: "i1", Project: "p1"}

	got, err := gv.Verify(context.Background(), Lesson{Kind: LessonSkill, Check: "make"}, scope)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.Verified || !got.Ran || got.Detail != "exit 0" {
		t.Fatalf("verdict not carried through: %+v", got)
	}

	events := sink.Events()
	if len(events) != 2 {
		t.Fatalf("want start+end on the spine, got %d events: %+v", len(events), events)
	}
	for _, want := range []string{dispatch.EventStart, dispatch.EventEnd} {
		if !hasEvent(events, want, VerifyAction, scope) {
			t.Fatalf("missing %s for %s in scope %+v: %+v", want, VerifyAction, scope, events)
		}
	}
}

// TestGovernedVerifierAdmissionDeniedIsUnproven confirms that when the run's grant
// does not admit the verification, it is reported unproven (Ran=false) rather than
// broken or a hard error: a declined verification is absence of evidence, not
// evidence the skill is wrong, so the skill is kept tagged unverified, not dropped.
func TestGovernedVerifierAdmissionDeniedIsUnproven(t *testing.T) {
	sink := &dispatch.MemorySink{}
	inner := fakeVerifier{v: Verdict{Verified: true, Ran: true}} // would pass if it ran
	gv := NewGovernedVerifier(inner,
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithEventSink(sink),
	)
	// A grant that admits some other action but not the verification action.
	ctx := capability.Into(context.Background(), capability.NewGrant("bash"))

	got, err := gv.Verify(ctx, Lesson{Kind: LessonSkill, Check: "make"}, state.Scope{})
	if err != nil {
		t.Fatalf("a denied verification must not be a hard error: %v", err)
	}
	if got.Ran || got.Verified {
		t.Fatalf("denied check must be unproven, got %+v", got)
	}
	if !hasEvent(sink.Events(), dispatch.EventRejected, VerifyAction, state.Scope{}) {
		t.Fatalf("a denied check must be recorded as rejected on the spine: %+v", sink.Events())
	}
}

// TestGovernedVerifierAdmittedActionRuns confirms a grant that names the verification
// action lets the check through to the inner verifier.
func TestGovernedVerifierAdmittedActionRuns(t *testing.T) {
	gv := NewGovernedVerifier(
		fakeVerifier{v: Verdict{Verified: true, Ran: true}},
		dispatch.WithAdmitter(capability.Admitter{}),
	)
	ctx := capability.Into(context.Background(), capability.NewGrant(VerifyAction))
	got, err := gv.Verify(ctx, Lesson{Kind: LessonSkill, Check: "make"}, state.Scope{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.Verified || !got.Ran {
		t.Fatalf("granted check should run and verify, got %+v", got)
	}
}

// TestGovernedVerifierCancelledIsHardError confirms a cancelled context surfaces as an
// error, matching the inner verifier, so a torn-down run does not quietly keep skills.
func TestGovernedVerifierCancelledIsHardError(t *testing.T) {
	gv := NewGovernedVerifier(fakeVerifier{err: context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gv.Verify(ctx, Lesson{Kind: LessonSkill, Check: "make"}, state.Scope{}); err == nil {
		t.Fatal("a cancelled context must surface as an error")
	}
}

// hasEvent reports whether events contains one of type typ for action under scope.
func hasEvent(events []dispatch.Event, typ, action string, scope state.Scope) bool {
	for _, e := range events {
		if e.Type == typ && e.Action == action && e.Scope == scope {
			return true
		}
	}
	return false
}
