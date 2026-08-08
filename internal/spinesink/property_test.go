package spinesink_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// genDispatchEvent draws a dispatch lifecycle event across the full field
// space, including empty optional fields, so the translation property covers
// every present/absent combination.
func genDispatchEvent(rt *rapid.T) dispatch.Event {
	return dispatch.Event{
		Type:   rapid.SampledFrom([]string{dispatch.EventStart, dispatch.EventEnd, dispatch.EventRejected}).Draw(rt, "type"),
		Action: rapid.StringMatching(`[a-z][a-z0-9_.]{0,15}`).Draw(rt, "action"),
		Call:   rapid.Int64Range(1, 1_000_000).Draw(rt, "call"),
		Scope: state.Scope{
			Instance:  rapid.StringMatching(`[a-z]{0,6}`).Draw(rt, "instance"),
			Project:   rapid.StringMatching(`[a-z]{0,6}`).Draw(rt, "project"),
			Workspace: rapid.StringMatching(`[a-z]{0,6}`).Draw(rt, "workspace"),
		},
		Trust: rapid.SampledFrom([]string{"", "trusted", "model", "external"}).Draw(rt, "trust"),
		Goal:  rapid.StringMatching(`([a-z0-9]{4,12})?`).Draw(rt, "goal"),
		At:    rapid.Int64Range(0, 4_102_444_800_000_000_000).Draw(rt, "at"), // up to year 2100
		Err:   rapid.SampledFrom([]string{"", "budget_exceeded", "needs_approval"}).Draw(rt, "err"),
		Code:  rapid.SampledFrom([]string{"", "capability_denied", "over_budget", "approval_required"}).Draw(rt, "code"),
	}
}

// Property: Append translates any dispatch event into exactly one spine event
// that preserves the type, the dispatcher's timestamp, and the action/call
// correlation, and carries each optional field (trust, error class, goal,
// scope) exactly when it is set. Nothing is lost and nothing is invented.
func TestProp_SinkTranslationPreservesEvent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		log := spine.NewMemoryLog()
		sink := spinesink.New(log, "run-p")

		e := genDispatchEvent(rt)
		if err := sink.Append(ctx, e); err != nil {
			rt.Fatalf("append: %v", err)
		}

		events, err := log.Read(ctx, spine.Query{Stream: "run-p"})
		if err != nil {
			rt.Fatalf("read: %v", err)
		}
		if len(events) != 1 {
			rt.Fatalf("got %d spine events, want 1", len(events))
		}
		got := events[0]

		if got.Type != e.Type {
			rt.Fatalf("type = %q, want %q", got.Type, e.Type)
		}
		if got.Actor != spine.ActorAgent {
			rt.Fatalf("actor = %q, want agent", got.Actor)
		}
		if want := time.Unix(0, e.At).UTC(); !got.Time.Equal(want) {
			rt.Fatalf("time = %v, want the dispatcher's %v", got.Time, want)
		}
		if got.Payload["action"] != e.Action {
			rt.Fatalf("payload action = %v, want %q", got.Payload["action"], e.Action)
		}
		if fmt.Sprint(got.Payload["call"]) != strconv.FormatInt(e.Call, 10) {
			rt.Fatalf("payload call = %v, want %d", got.Payload["call"], e.Call)
		}

		// Optional fields appear exactly when set.
		checkOptional := func(key, want string) {
			v, present := got.Payload[key]
			if present != (want != "") {
				rt.Fatalf("payload %q present = %v, want %v", key, present, want != "")
			}
			if present && v != want {
				rt.Fatalf("payload %q = %v, want %q", key, v, want)
			}
		}
		checkOptional("trust", e.Trust)
		checkOptional("error_class", e.Err)
		checkOptional("error_code", e.Code)
		checkOptional("goal", e.Goal)

		_, scopePresent := got.Payload["scope"]
		if scopePresent != (e.Scope != (state.Scope{})) {
			rt.Fatalf("payload scope present = %v for scope %+v", scopePresent, e.Scope)
		}
	})
}

// Property: every egress decision lands as exactly one net.egress event on the
// run's stream, with the verdict string matching the boolean and the host and
// reason carried through verbatim.
func TestProp_EgressDecisionRecordedVerbatim(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		log := spine.NewMemoryLog()
		sink := spinesink.NewEgress(log, "run-e")

		decisions := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) netguard.Decision {
			return netguard.Decision{
				Host:    rapid.StringMatching(`[a-z0-9.-]{1,20}`).Draw(t, "host"),
				Allowed: rapid.Bool().Draw(t, "allowed"),
				Reason:  rapid.StringMatching(`[a-z ]{0,20}`).Draw(t, "reason"),
			}
		}), 1, 10).Draw(rt, "decisions")

		for _, d := range decisions {
			sink.Observe(d)
		}

		events, err := log.Read(ctx, spine.Query{Stream: "run-e"})
		if err != nil {
			rt.Fatalf("read: %v", err)
		}
		if len(events) != len(decisions) {
			rt.Fatalf("got %d events for %d decisions", len(events), len(decisions))
		}
		for i, d := range decisions {
			got := events[i]
			if got.Type != "net.egress" {
				rt.Fatalf("event %d type = %q, want net.egress", i, got.Type)
			}
			verdict := "blocked"
			if d.Allowed {
				verdict = "allowed"
			}
			if got.Payload["verdict"] != verdict {
				rt.Fatalf("event %d verdict = %v, want %q", i, got.Payload["verdict"], verdict)
			}
			if got.Payload["host"] != d.Host || got.Payload["reason"] != d.Reason {
				rt.Fatalf("event %d carries host=%v reason=%v, want %q/%q", i, got.Payload["host"], got.Payload["reason"], d.Host, d.Reason)
			}
		}
	})
}
