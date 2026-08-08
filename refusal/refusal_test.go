package refusal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/refusal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// gate refuses the actions it is told to, with the rule it is told to name, and admits
// everything else. It stands in for the real gates (capability admission, the containment
// hook, an approval denial), all of which refuse through this one interface.
type gate struct {
	deny map[string]string // action -> the fault code that refuses it
}

func (g gate) Admit(_ context.Context, a dispatch.Action) error {
	if code, ok := g.deny[a.Name]; ok {
		return fault.New(fault.Forbidden, code, "not granted: "+a.Name)
	}
	return nil
}

func noWork(context.Context) (dispatch.Metering, error) { return dispatch.Metering{}, nil }

// TestRefusalsAreReadBackOffTheWaistsOwnRecord is the wire this whole thing rests on. The
// refusals are not reported by the run and not passed in memory: a real dispatcher refuses
// real actions, the sink writes them to the spine, and the probe reads them back as the
// goal reconciler will see them. If any layer between here and there stopped carrying which
// rule refused, this test is what notices.
func TestRefusalsAreReadBackOffTheWaistsOwnRecord(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	d := dispatch.New(
		dispatch.WithEventSink(spinesink.New(log, "g")),
		dispatch.WithAdmitter(gate{deny: map[string]string{
			"write_file":   "capability_denied",
			"bash":         "capability_denied",
			"mcp.fs.write": "capability_denied",
		}}),
	)

	// A run that met one gate, was told no, and came back by another door twice, doing a
	// little admitted work in between so the record is not only refusals.
	for _, action := range []string{"write_file", "read_file", "bash", "read_file", "mcp.fs.write"} {
		_ = d.Govern(ctx, dispatch.Action{Name: action}, noWork)
	}

	got, err := refusal.NewSpineProbe(log).Refusals(ctx, resource.Resource{Name: "g"})
	if err != nil {
		t.Fatalf("refusals: %v", err)
	}
	want := []goal.Refusal{
		{Rule: "capability_denied", Action: "write_file"},
		{Rule: "capability_denied", Action: "bash"},
		{Rule: "capability_denied", Action: "mcp.fs.write"},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d refusals, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("refusal %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// And the record those three amount to, which is the sentence the goal stops on.
	v, stop := goal.ReadRefusals(got)
	if !stop || !v.Routed || v.Rule != "capability_denied" {
		t.Fatalf("verdict = %+v, stop = %v; want a substitution under capability_denied", v, stop)
	}
}

// TestAdmittedRunHasNothingAgainstIt is the case that must stay silent: a run nobody
// refused reads back no refusals, so the detector cannot stop it.
func TestAdmittedRunHasNothingAgainstIt(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	d := dispatch.New(dispatch.WithEventSink(spinesink.New(log, "g")))
	for range 5 {
		if err := d.Govern(ctx, dispatch.Action{Name: "read_file"}, noWork); err != nil {
			t.Fatalf("govern: %v", err)
		}
	}
	got, err := refusal.NewSpineProbe(log).Refusals(ctx, resource.Resource{Name: "g"})
	if err != nil {
		t.Fatalf("refusals: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read %d refusals from a run nobody refused: %+v", len(got), got)
	}
}

// TestFailedActionIsNotARefusal keeps the two apart. An action that was admitted and then
// failed is work that went wrong; an action that was refused never ran. Counting the first
// as the second would stop a run for having a flaky tool.
func TestFailedActionIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	d := dispatch.New(dispatch.WithEventSink(spinesink.New(log, "g")))
	boom := func(context.Context) (dispatch.Metering, error) {
		return dispatch.Metering{}, fault.New(fault.Transient, "tool_failed", "the tool crashed")
	}
	for range 5 {
		if err := d.Govern(ctx, dispatch.Action{Name: "bash"}, boom); err == nil {
			t.Fatal("the failing work reported success")
		}
	}
	got, err := refusal.NewSpineProbe(log).Refusals(ctx, resource.Resource{Name: "g"})
	if err != nil {
		t.Fatalf("refusals: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read %d refusals from a run whose actions were admitted and then failed: %+v", len(got), got)
	}
}

// TestReadsOnlyThisGoalsStream is what makes one probe safe to serve a fan-out. A child's
// refusals belong to the child, and folding a sibling's in would stop a run for doors
// somebody else tried.
func TestReadsOnlyThisGoalsStream(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	deny := gate{deny: map[string]string{"write_file": "capability_denied"}}
	for _, stream := range []string{"child-a", "child-b"} {
		d := dispatch.New(dispatch.WithEventSink(spinesink.New(log, stream)), dispatch.WithAdmitter(deny))
		_ = d.Govern(ctx, dispatch.Action{Name: "write_file"}, noWork)
	}
	p := refusal.NewSpineProbe(log)
	for _, stream := range []string{"child-a", "child-b"} {
		got, err := p.Refusals(ctx, resource.Resource{Name: stream})
		if err != nil {
			t.Fatalf("refusals(%s): %v", stream, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: read %d refusals, want its own one: %+v", stream, len(got), got)
		}
	}
}

// TestUnreadableLogFailsTransient pins the direction the probe fails in. A log that could
// not be read says nothing about whether the run was refused, and a terminal classification
// here would stall a healthy goal over a momentary read.
func TestUnreadableLogFailsTransient(t *testing.T) {
	p := refusal.NewSpineProbe(brokenLog{})
	_, err := p.Refusals(context.Background(), resource.Resource{Name: "g"})
	if err == nil {
		t.Fatal("an unreadable log read as a run with nothing against it")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Errorf("class = %q, want %q", got, fault.Transient)
	}
}

// TestNoLogWiredIsTerminal is the misconfiguration, which is not a momentary failure and
// must not be retried forever.
func TestNoLogWiredIsTerminal(t *testing.T) {
	_, err := refusal.NewSpineProbe(nil).Refusals(context.Background(), resource.Resource{Name: "g"})
	if err == nil {
		t.Fatal("a probe with no log wired reported a clean run")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Errorf("class = %q, want %q", got, fault.Terminal)
	}
}

// brokenLog fails every read, standing in for a durable log that is briefly unreachable.
type brokenLog struct{ spine.Log }

func (brokenLog) Read(context.Context, spine.Query) ([]spine.Event, error) {
	return nil, errors.New("the log is unreachable")
}
