package dispatch_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// orderHook records the order of Before/After calls and can fail its Before.
type orderHook struct {
	name string
	fail bool
	log  *[]string
}

func (h *orderHook) Before(context.Context, dispatch.Action) error {
	*h.log = append(*h.log, h.name+".before")
	if h.fail {
		return fault.New(fault.Terminal, "blocked", "denied")
	}
	return nil
}

func (h *orderHook) After(context.Context, dispatch.Action, dispatch.Metering, error) {
	*h.log = append(*h.log, h.name+".after")
}

type recordHook struct {
	before func(context.Context, dispatch.Action) error
	afters int
}

func (h *recordHook) Before(ctx context.Context, a dispatch.Action) error {
	if h.before != nil {
		return h.before(ctx, a)
	}
	return nil
}

func (h *recordHook) After(context.Context, dispatch.Action, dispatch.Metering, error) { h.afters++ }

type denyAdmitter struct{ err error }

func (d denyAdmitter) Admit(context.Context, dispatch.Action) error { return d.err }

// okWork is a unit of work that records it ran and reports tokens.
func okWork(called *bool, tokens int) func(context.Context) (dispatch.Metering, error) {
	return func(context.Context) (dispatch.Metering, error) {
		*called = true
		return dispatch.Metering{Tokens: tokens}, nil
	}
}

func TestGovernHappyPath(t *testing.T) {
	called := false
	sink := &dispatch.MemorySink{}
	clk := clock.NewManual(time.Unix(1000, 0))
	hook := &recordHook{}

	d := dispatch.New(
		dispatch.WithEventSink(sink),
		dispatch.WithClock(clk),
		dispatch.WithHook(hook),
	)

	if err := d.Govern(context.Background(), dispatch.Action{Name: "echo"}, okWork(&called, 7)); err != nil {
		t.Fatalf("Govern: %v", err)
	}
	if !called {
		t.Fatal("work was not called")
	}

	evs := sink.Events()
	if len(evs) != 2 || evs[0].Type != dispatch.EventStart || evs[1].Type != dispatch.EventEnd {
		t.Fatalf("events = %+v, want start+end", evs)
	}
	if evs[1].Err != "" {
		t.Fatalf("end event Err = %q, want empty on success", evs[1].Err)
	}
	if evs[0].At != clk.Now().UnixNano() {
		t.Fatal("event timestamp did not come from the injected clock")
	}
	if hook.afters != 1 {
		t.Fatalf("After hooks ran %d times, want 1", hook.afters)
	}
}

func TestGovernAdmissionRejects(t *testing.T) {
	called := false
	sink := &dispatch.MemorySink{}
	hook := &recordHook{}

	d := dispatch.New(
		dispatch.WithAdmitter(denyAdmitter{err: fault.New(fault.NeedsApproval, "approval_required", "needs a human")}),
		dispatch.WithEventSink(sink),
		dispatch.WithHook(hook),
	)

	err := d.Govern(context.Background(), dispatch.Action{Name: "rm-rf"}, okWork(&called, 0))
	if err == nil {
		t.Fatal("expected an admission rejection error")
	}
	if called {
		t.Fatal("work must not run when admission rejects")
	}
	evs := sink.Events()
	if len(evs) != 1 || evs[0].Type != dispatch.EventRejected {
		t.Fatalf("events = %+v, want a single rejected event", evs)
	}
	if evs[0].Err != string(fault.NeedsApproval) {
		t.Fatalf("rejected class = %q, want %q", evs[0].Err, fault.NeedsApproval)
	}
	if hook.afters != 1 {
		t.Fatalf("After hooks ran %d times, want 1 even on rejection", hook.afters)
	}
}

func TestGovernBeforeHookRejects(t *testing.T) {
	called := false
	sink := &dispatch.MemorySink{}
	blocked := errors.New("blocked by policy")

	hook := &recordHook{before: func(context.Context, dispatch.Action) error { return blocked }}
	d := dispatch.New(
		dispatch.WithEventSink(sink),
		dispatch.WithHook(hook),
	)

	err := d.Govern(context.Background(), dispatch.Action{Name: "deploy"}, okWork(&called, 0))
	if !errors.Is(err, blocked) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
	if called {
		t.Fatal("work must not run when a Before hook rejects")
	}
	if hook.afters != 0 {
		t.Fatalf("After ran %d times for a hook whose own Before failed, want 0", hook.afters)
	}
	if evs := sink.Events(); len(evs) != 1 || evs[0].Type != dispatch.EventRejected {
		t.Fatalf("events = %+v, want a single rejected event", evs)
	}
}

func TestGovernHookUnwindIsReverseAndOnlyEntered(t *testing.T) {
	var order []string
	called := false
	d := dispatch.New(
		dispatch.WithHook(&orderHook{name: "h1", log: &order}),
		dispatch.WithHook(&orderHook{name: "h2", fail: true, log: &order}),
	)

	if err := d.Govern(context.Background(), dispatch.Action{Name: "x"}, okWork(&called, 0)); err == nil {
		t.Fatal("expected h2's Before to reject")
	}
	if called {
		t.Fatal("work must not run when a Before hook rejects")
	}
	// h1 entered (After runs); h2's Before failed (no After). After is reverse.
	want := []string{"h1.before", "h2.before", "h1.after"}
	if !slices.Equal(order, want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
}

func TestGovernWorkFailureClassified(t *testing.T) {
	sink := &dispatch.MemorySink{}
	d := dispatch.New(dispatch.WithEventSink(sink))

	err := d.Govern(context.Background(), dispatch.Action{Name: "fetch"}, func(context.Context) (dispatch.Metering, error) {
		return dispatch.Metering{}, fault.New(fault.Transient, "upstream_503", "try again")
	})
	if fault.Classify(err) != fault.Transient {
		t.Fatalf("class = %v, want transient", fault.Classify(err))
	}
	evs := sink.Events()
	if len(evs) != 2 || evs[1].Type != dispatch.EventEnd {
		t.Fatalf("events = %+v, want start+end", evs)
	}
	if evs[1].Err != string(fault.Transient) {
		t.Fatalf("end event Err = %q, want transient", evs[1].Err)
	}
}

func TestGovernZeroConfig(t *testing.T) {
	called := false
	d := dispatch.New()
	if err := d.Govern(context.Background(), dispatch.Action{Name: "noop"}, okWork(&called, 0)); err != nil {
		t.Fatalf("zero-config govern failed: %v", err)
	}
	if !called {
		t.Fatal("work did not run")
	}
}
