package learn

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/state"
)

// TestGovernedDistillerRecordsOnSpine confirms a distillation flows through the
// dispatch waist: the inner lessons are returned unchanged, and the model call is
// bracketed by start/end events on the spine under the stable action name and scope.
func TestGovernedDistillerRecordsOnSpine(t *testing.T) {
	sink := &dispatch.MemorySink{}
	inner := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "x"}}}
	gd := NewGovernedDistiller(inner, dispatch.WithEventSink(sink))
	scope := state.Scope{Instance: "i1", Project: "p1"}

	got, err := gd.Distill(context.Background(), Outcome{Objective: "o", Scope: scope})
	if err != nil {
		t.Fatalf("distill: %v", err)
	}
	if len(got) != 1 || got[0].Title != "x" {
		t.Fatalf("lessons not carried through: %+v", got)
	}
	for _, want := range []string{dispatch.EventStart, dispatch.EventEnd} {
		if !hasEvent(sink.Events(), want, DistillAction, scope) {
			t.Fatalf("missing %s for %s in scope %+v: %+v", want, DistillAction, scope, sink.Events())
		}
	}
}

// TestGovernedDistillerAdmissionDeniedCapturesNothing confirms that when the run's
// grant does not admit the distillation, it never runs: nothing is captured and no
// hard error is raised, because governance opting out is not a failure.
func TestGovernedDistillerAdmissionDeniedCapturesNothing(t *testing.T) {
	sink := &dispatch.MemorySink{}
	inner := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "x"}}} // would capture if it ran
	gd := NewGovernedDistiller(inner,
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithEventSink(sink),
	)
	ctx := capability.Into(context.Background(), capability.NewGrant("bash")) // not DistillAction

	got, err := gd.Distill(ctx, Outcome{Scope: state.Scope{}})
	if err != nil {
		t.Fatalf("a declined distillation must not be a hard error: %v", err)
	}
	if got != nil {
		t.Fatalf("declined distillation must capture nothing, got %+v", got)
	}
	if !hasEvent(sink.Events(), dispatch.EventRejected, DistillAction, state.Scope{}) {
		t.Fatalf("a declined distillation must be recorded as rejected: %+v", sink.Events())
	}
}

// TestGovernedDistillerInnerErrorPropagates confirms a failure inside the distiller
// (e.g. a malformed model reply) surfaces rather than being swallowed, so a broken
// distiller stays visible.
func TestGovernedDistillerInnerErrorPropagates(t *testing.T) {
	gd := NewGovernedDistiller(&fakeDistiller{err: fault.New(fault.Terminal, "bad_json", "malformed")})
	if _, err := gd.Distill(context.Background(), Outcome{}); err == nil {
		t.Fatal("a broken distiller must surface its error, not be swallowed")
	}
}
