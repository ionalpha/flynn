package externagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/tools"
)

// spineSink collects the dispatch lifecycle events an episode's bridged calls wrote to the
// run's event spine.
type spineSink struct {
	mu     sync.Mutex
	events []dispatch.Event
}

func (s *spineSink) Append(_ context.Context, e dispatch.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *spineSink) appended() []dispatch.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dispatch.Event{}, s.events...)
}

// epoch is a fixed instant the brake's rate window is measured against, so the test never
// reads the wall clock.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestDriverBindsTheWholeWaistToAnExternalEpisode is the property the external backend
// exists to preserve: swapping the loop for someone else's CLI must not drop any part of the
// governance. Every ingredient of the waist is bound here at once (the containment gate, the
// event spine, the run's brake), and a bridged call the harness makes must cross all of them,
// exactly as a native loop's call would. If any one were dropped, an external run would
// silently be less governed than a native one while producing a record that looks the same.
func TestDriverBindsTheWholeWaistToAnExternalEpisode(t *testing.T) {
	workdir := t.TempDir()
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	spawner := scriptSpawner(func(ep Episode, inv Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = bridgeClient(ep.Bridge, "write", `{"path":"governed.txt","content":"hi"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
		_ = os.WriteFile(filepath.Join(ep.Workdir, inv.LastMessageFile), []byte("done"), 0o644)
	})

	sink := &spineSink{}
	d := NewDriver(NewCodex("", nil), spawner, workdir)
	spec := specWithFullWaist(t, sb, sink)

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gspec := goal.Spec{Objective: "write a file", Grant: []string{"write", "read"}}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The effect landed, so the containment gate admitted the call rather than refusing it.
	if b, err := os.ReadFile(filepath.Join(workdir, "governed.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("the bridged write did not land through the gated waist: %v / %q", err, string(b))
	}
	// The call's lifecycle is on the run's spine, so the admission decision is part of the
	// sealed history and not merely the live trace.
	got := sink.appended()
	if len(got) == 0 {
		t.Fatal("no dispatch lifecycle reached the event spine: the external run's admissions would not be recorded")
	}
	var sawWrite bool
	for _, e := range got {
		if e.Action == "write" {
			sawWrite = true
		}
	}
	if !sawWrite {
		t.Errorf("the bridged write is missing from the spine: %+v", got)
	}
	// The brake was bound too: the run's halt state is what a bridged call is refused against.
	if s := d.Steering(); s.BridgeCalls != 1 {
		t.Errorf("bridged calls = %d, want 1 (the probe is the run's own instrument, not counted)", s.BridgeCalls)
	}
}

// TestDriverRefusesABridgedCallOnAHaltedRun proves the run's brake reaches an external
// harness. The kill-switch is engaged before the episode runs, so every bridged call the CLI
// makes is refused at the waist, and the effect never lands. The brake sits outside the loop
// on purpose: selecting someone else's harness must never be a way to escape a halt.
func TestDriverRefusesABridgedCallOnAHaltedRun(t *testing.T) {
	workdir := t.TempDir()
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		// The probe is exempt from the brake, so a halted run does not read as the harness
		// refusing to cooperate. This call must still reach the tool.
		satisfyProbe(ep)
		_, _ = bridgeClient(ep.Bridge, "write", `{"path":"halted.txt","content":"x"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})

	d := NewDriver(NewCodex("", nil), spawner, workdir)
	spec := specWithFullWaist(t, sb, &spineSink{})
	spec.Brakes.Switch().Engage("g1", "the operator halted the run")

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gspec := goal.Spec{Objective: "write a file", Grant: []string{"write", "read"}}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("a refused action must not fail the episode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "halted.txt")); !os.IsNotExist(err) {
		t.Error("an external harness wrote through the bridge on a halted run")
	}
}

// specWithFullWaist builds the driver Spec with every governance ingredient bound: the
// toolset, the containment gate, the event spine, and the run's brake on a manual clock.
func specWithFullWaist(t *testing.T, sb *sandbox.Local, sink dispatch.EventSink) driver.Spec {
	t.Helper()
	return driver.Spec{
		Tools:     tools.New(sb).Tools(),
		Sandbox:   sb,
		EventSink: sink,
		Brakes:    brakes.NewHook(brakes.Limits{}, nil, brakes.WithClock(clock.NewManual(epoch))),
	}
}

// TestProbeToolRefusesAnUnreadableArgument proves a call whose argument cannot be decoded is
// an error rather than a silent pass. The probe settles on the tool's own record of being
// called with the episode's nonce, so a call carrying nothing readable must not move it: a
// harness that dispatched garbage did not follow the instruction it was given.
func TestProbeToolRefusesAnUnreadableArgument(t *testing.T) {
	tool := NewProbeTool("the-real-nonce")
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"nonce":`)); err == nil {
		t.Fatal("an argument that cannot be decoded must be reported as an error")
	}
	if tool.Called() {
		t.Error("an undecodable call satisfied the probe")
	}
}
