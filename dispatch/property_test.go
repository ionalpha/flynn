package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// traceHook records its Before/After calls (and optionally fails Before) so a
// property can check the waist's acquire/release pairing.
type traceHook struct {
	id         int
	failBefore bool
	trace      *[]string
	afterErr   *[]error
}

func (h *traceHook) Before(_ context.Context, _ dispatch.Action) error {
	*h.trace = append(*h.trace, fmt.Sprintf("before:%d", h.id))
	if h.failBefore {
		return errors.New("hook refused")
	}
	return nil
}

func (h *traceHook) After(_ context.Context, _ dispatch.Action, _ dispatch.Metering, err error) {
	*h.trace = append(*h.trace, fmt.Sprintf("after:%d", h.id))
	*h.afterErr = append(*h.afterErr, err)
}

// Property: whatever fails - a Before hook, admission, or the work itself -
// After runs for exactly the hooks whose Before succeeded, in reverse order,
// like a defer stack. No hook is unwound that never entered, and none that
// entered is skipped.
func TestProp_HooksUnwindInReverseForEnteredSet(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nHooks := rapid.IntRange(0, 5).Draw(rt, "nHooks")
		failAt := rapid.IntRange(-1, nHooks-1).Draw(rt, "failAt") // -1: no hook fails
		admitFail := rapid.Bool().Draw(rt, "admitFail")
		workFail := rapid.Bool().Draw(rt, "workFail")

		var trace []string
		var afterErrs []error
		opts := []dispatch.Option{}
		for i := range nHooks {
			opts = append(opts, dispatch.WithHook(&traceHook{
				id: i, failBefore: i == failAt, trace: &trace, afterErr: &afterErrs,
			}))
		}
		if admitFail {
			opts = append(opts, dispatch.WithAdmitter(denyAll{}))
		}
		d := dispatch.New(opts...)

		workRan := false
		err := d.Govern(context.Background(), dispatch.Action{Name: "act"}, func(context.Context) (dispatch.Metering, error) {
			workRan = true
			if workFail {
				return dispatch.Metering{}, errors.New("work failed")
			}
			return dispatch.Metering{}, nil
		})

		entered := nHooks
		if failAt >= 0 {
			entered = failAt // hooks before the failing one entered; the failing one did not
		}
		rejected := failAt >= 0 || admitFail
		if workRan == rejected {
			rt.Fatalf("workRan = %v with rejected = %v", workRan, rejected)
		}
		if wantErr := rejected || workFail; (err != nil) != wantErr {
			rt.Fatalf("Govern err = %v, want error: %v", err, wantErr)
		}

		// Befores 0..entered (inclusive of the failing hook's own Before) ran in
		// order; exactly `entered` Afters follow, in reverse.
		befores := entered
		if failAt >= 0 {
			befores++ // the failing hook's Before ran and returned the error
		}
		if want := befores + entered; len(trace) != want {
			rt.Fatalf("trace = %v, want %d before + %d after calls", trace, befores, entered)
		}
		for i := range befores {
			if trace[i] != fmt.Sprintf("before:%d", i) {
				rt.Fatalf("trace[%d] = %q, want before:%d (trace %v)", i, trace[i], i, trace)
			}
		}
		for i := range entered {
			want := fmt.Sprintf("after:%d", entered-1-i)
			if trace[befores+i] != want {
				rt.Fatalf("trace[%d] = %q, want %s (trace %v)", befores+i, trace[befores+i], want, trace)
			}
		}
		// Every unwound hook saw the same error Govern returned.
		for i, aerr := range afterErrs {
			if !errors.Is(aerr, err) {
				rt.Fatalf("After %d saw err %v, Govern returned %v", i, aerr, err)
			}
		}
	})
}

// denyAll rejects every action.
type denyAll struct{}

func (denyAll) Admit(context.Context, dispatch.Action) error {
	return fault.New(fault.BudgetExceeded, "deny_all", "denied")
}

// Property: over any sequence of governed calls with arbitrary outcomes, the
// event stream is well formed: each invocation emits either start+end or a
// single rejection, all three share that invocation's correlation id, ids are
// strictly increasing, and end events carry the fault class exactly when the
// work failed.
func TestProp_EventStreamPairsStartEndByCall(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		outcomes := rapid.SliceOfN(rapid.SampledFrom([]string{"ok", "workfail", "reject"}), 1, 10).Draw(rt, "outcomes")

		sink := &dispatch.MemorySink{}
		allow := dispatch.New(dispatch.WithEventSink(sink))
		deny := dispatch.New(dispatch.WithEventSink(sink), dispatch.WithAdmitter(denyAll{}))

		var lastCall int64
		for i, outcome := range outcomes {
			d := allow
			if outcome == "reject" {
				d = deny
			}
			name := fmt.Sprintf("act%d", i)
			before := len(sink.Events())
			_ = d.Govern(context.Background(), dispatch.Action{Name: name}, func(context.Context) (dispatch.Metering, error) {
				if outcome == "workfail" {
					return dispatch.Metering{}, errors.New("boom")
				}
				return dispatch.Metering{Tokens: 1}, nil
			})
			evs := sink.Events()[before:]

			switch outcome {
			case "reject":
				if len(evs) != 1 || evs[0].Type != dispatch.EventRejected {
					rt.Fatalf("outcome %q emitted %+v, want one rejection", outcome, evs)
				}
				if evs[0].Err == "" {
					rt.Fatalf("rejection carries no fault class: %+v", evs[0])
				}
			default:
				if len(evs) != 2 || evs[0].Type != dispatch.EventStart || evs[1].Type != dispatch.EventEnd {
					rt.Fatalf("outcome %q emitted %+v, want start+end", outcome, evs)
				}
				if evs[0].Call != evs[1].Call {
					rt.Fatalf("start call %d != end call %d", evs[0].Call, evs[1].Call)
				}
				if gotErr := evs[1].Err != ""; gotErr != (outcome == "workfail") {
					rt.Fatalf("outcome %q: end.Err = %q", outcome, evs[1].Err)
				}
				if evs[0].At > evs[1].At {
					rt.Fatalf("start at %d after end at %d", evs[0].At, evs[1].At)
				}
			}
			for _, e := range evs {
				if e.Action != name {
					rt.Fatalf("event action = %q, want %q", e.Action, name)
				}
			}
			// Correlation ids are strictly increasing per dispatcher; both dispatchers
			// here share a sink but not a counter, so compare within the allow path only.
			if outcome != "reject" {
				if evs[0].Call <= lastCall {
					rt.Fatalf("call id %d not greater than previous %d", evs[0].Call, lastCall)
				}
				lastCall = evs[0].Call
			}
		}
	})
}
