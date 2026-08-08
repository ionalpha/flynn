package refusal_test

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/refusal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// Property: whatever a run does, the probe reads back exactly the actions the waist
// refused, in the order it refused them, each naming the rule that spoke. Nothing admitted
// appears, nothing refused is dropped, and the order is the record's.
//
// The run is driven through a real dispatcher rather than by writing events, so the
// property covers the whole wire: the waist classifying a refusal, the sink encoding it,
// the session projection decoding it, and the probe reading it. A layer that stopped
// carrying which rule refused would fail here rather than at the layer that dropped it,
// which is the point of asserting it end to end.
func TestProp_ProbeReadsExactlyTheRefusedActions(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		log := spine.NewMemoryLog()

		// The gates in play and the actions they refuse, drawn so a run can meet several
		// walls, one wall many times, or none at all.
		rules := rapid.SampledFrom([]string{"capability_denied", "containment_unavailable", "approval_required"})
		actions := rapid.SampledFrom([]string{"read_file", "write_file", "bash", "net.dial", "mcp.fs.write"})
		deny := map[string]string{}
		for _, act := range rapid.SliceOfN(actions, 0, 5).Draw(rt, "denied") {
			deny[act] = rules.Draw(rt, "rule:"+act)
		}

		d := dispatch.New(
			dispatch.WithEventSink(spinesink.New(log, "g")),
			dispatch.WithAdmitter(gate{deny: deny}),
		)

		// Half the drawn actions fail once admitted, so an admitted-then-failed action has
		// a chance to be miscounted as a refusal if the probe ever keyed on the error
		// rather than on the rejection.
		var want []goal.Refusal
		for _, act := range rapid.SliceOfN(actions, 0, 20).Draw(rt, "run") {
			work := noWork
			if rapid.Bool().Draw(rt, "fails:"+act) {
				work = failingWork
			}
			_ = d.Govern(ctx, dispatch.Action{Name: act}, work)
			if rule, refused := deny[act]; refused {
				want = append(want, goal.Refusal{Rule: rule, Action: act})
			}
		}

		got, err := refusal.NewSpineProbe(log).Refusals(ctx, resource.Resource{Name: "g"})
		if err != nil {
			rt.Fatalf("refusals: %v", err)
		}
		if len(got) != len(want) {
			rt.Fatalf("read %d refusals, want %d: got %+v, want %+v", len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				rt.Fatalf("refusal %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}

func failingWork(context.Context) (dispatch.Metering, error) {
	return dispatch.Metering{}, errors.New("the tool crashed")
}
