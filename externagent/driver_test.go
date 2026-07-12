package externagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/tools"
)

// goalResource builds a Goal resource carrying spec for the executor to decode.
func goalResource(t *testing.T, name string, spec goal.Spec, checkpoint json.RawMessage) resource.Resource {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: name, Spec: raw}
	if len(checkpoint) > 0 {
		st, err := json.Marshal(goal.Status{Checkpoint: checkpoint})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		r.Status = st
	}
	return r
}

// driverWith builds a Driver whose bridge serves the default toolset over a real
// sandbox in workdir, driven by the given scripted spawner.
func driverWith(t *testing.T, workdir string, spawner Spawner) (*Driver, driver.Spec) {
	t.Helper()
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	d := NewDriver(NewCodex("", nil), spawner, workdir)
	return d, driver.Spec{Tools: tools.New(sb).Tools()}
}

var nonceRe = regexp.MustCompile(`nonce="([^"]+)"`)

// probeNonce recovers the nonce the driver minted for an episode from the instruction it
// was handed. A real harness reads it the same way: out of the turn text, since that is
// the only channel it has. It returns empty when the instruction carries none, which
// makes the probe fail rather than the test panic from a spawner goroutine.
func probeNonce(ep Episode) string {
	for _, instr := range ep.Probes {
		if m := nonceRe.FindStringSubmatch(instr); m != nil {
			return m[1]
		}
	}
	return ""
}

// satisfyProbe makes the bridged probe call a compliant harness would make, so a test
// episode clears the required conformance probe and can get on with what it is testing.
// It runs on the episode's goroutine, so a failure surfaces as the probe not passing
// rather than as a t.Fatal from the wrong goroutine.
func satisfyProbe(ep Episode) {
	_, _ = bridgeClient(ep.Bridge, ProbeToolName, `{"nonce":"`+probeNonce(ep)+`"}`)
}

// TestDriverRunsEpisodeThroughWaist drives Build -> Execute end to end: the episode
// writes to the workspace through the bridge (governed by the goal's grant), the
// checkpoint records completion and the final message, and the stop evaluator
// converges.
func TestDriverRunsEpisodeThroughWaist(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, inv Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		ok, _ := bridgeClient(ep.Bridge, "write", `{"path":"out.txt","content":"hi"}`)
		_, _ = fmt.Fprintf(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"wrote (ok=%v)"}}`+"\n", ok)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
		_ = os.WriteFile(filepath.Join(ep.Workdir, inv.LastMessageFile), []byte("all done"), 0o644)
	})
	d, spec := driverWith(t, workdir, spawner)

	exec, stop, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gspec := goal.Spec{Objective: "write a file", StopCondition: "out.txt exists", Grant: []string{"write", "read"}, Model: "gpt-5-codex"}
	ckpt, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The bridged write landed through the waist.
	if b, err := os.ReadFile(filepath.Join(workdir, "out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("bridged write did not land: %v / %q", err, string(b))
	}

	// The stop evaluator converges on the completed episode with the final message.
	met, reason, err := stop.Met(context.Background(), gspec, goal.Status{Checkpoint: ckpt})
	if err != nil || !met {
		t.Fatalf("stop not met: met=%v err=%v", met, err)
	}
	if reason != "all done" {
		t.Errorf("reason not the final message: %q", reason)
	}
}

// TestDriverAccumulatesAttestedTiersAcrossEpisodes proves the run's tier tally is the
// sum of every episode's projected events, not just the last one's. The host reads this
// tally to declare on the sealed record how much of the run rests on the harness's own
// account, so an under-count would understate the unverified portion of the record.
func TestDriverAccumulatesAttestedTiersAcrossEpisodes(t *testing.T) {
	workdir := t.TempDir()
	// Each episode projects two attested events: an agent message and a turn completion.
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"step"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gspec := goal.Spec{Objective: "think", Model: "gpt-5-codex"}
	const episodes = 3
	for i := range episodes {
		// A fresh checkpoint each time, so every call runs a real episode rather than
		// short-circuiting on an already-done one.
		if _, err := exec.Execute(context.Background(), goalResource(t, fmt.Sprintf("g%d", i), gspec, nil)); err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
	}

	tiers := d.Tiers()
	if got := tiers[TierAttested]; got != 2*episodes {
		t.Errorf("attested tally = %d across %d episodes, want %d", got, episodes, 2*episodes)
	}
	// Tiers hands back a copy: mutating it must not corrupt the run's tally.
	tiers[TierAttested] = 999
	if got := d.Tiers()[TierAttested]; got != 2*episodes {
		t.Errorf("Tiers() leaked its internal map: tally now %d", got)
	}
}

// TestDriverGrantDeniesUngrantedTool proves the bridge still governs under the
// driver: a goal whose grant omits write cannot write through the bridge even though
// the episode tried, and the episode still completes.
func TestDriverGrantDeniesUngrantedTool(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = bridgeClient(ep.Bridge, "write", `{"path":"nope.txt","content":"x"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, _ := d.Build(spec)

	gspec := goal.Spec{Objective: "try to write", Grant: []string{"read"}} // no write
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "nope.txt")); !os.IsNotExist(err) {
		t.Errorf("ungranted write should not have created a file")
	}
}

// budgetStore builds an in-memory resource store with the budget kind registered, so
// a test can open a run's spend pool.
func budgetStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := budget.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

// TestDriverEnforcesBudgetCeiling proves the run's spend ceiling bounds an external
// harness's bridged tool calls exactly as it bounds a native loop: with the pool
// exhausted, a bridged write is refused at the waist and never touches the workspace,
// even though the episode attempted it and still completes.
func TestDriverEnforcesBudgetCeiling(t *testing.T) {
	workdir := t.TempDir()
	store := budgetStore(t)
	ledger := budget.NewLedger(store)

	// Open a pool for the goal id the episode runs under and exhaust it, so the first
	// bridged action is over budget.
	ctx := context.Background()
	if _, err := ledger.Open(ctx, "g1", resource.Scope{}, budget.Limits{Tokens: 1}); err != nil {
		t.Fatalf("open budget: %v", err)
	}
	if err := ledger.Charge(ctx, "g1", resource.Scope{}, dispatch.Metering{Tokens: 100}); err != nil {
		t.Fatalf("charge budget: %v", err)
	}

	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		// The probe is exempt from the spend ceiling, so an exhausted pool does not read as
		// the harness refusing to cooperate. This call must succeed even though the write
		// below is refused.
		satisfyProbe(ep)
		_, _ = bridgeClient(ep.Bridge, "write", `{"path":"over.txt","content":"x"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	spec.Budget = budget.NewHook(store)

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gspec := goal.Spec{Objective: "try to write", Grant: []string{"write", "read"}}
	if _, err := exec.Execute(ctx, goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "over.txt")); !os.IsNotExist(err) {
		t.Errorf("a bridged action over budget must be refused at the waist")
	}
}

// TestDriverResumesDoneCheckpoint proves a step whose checkpoint is already done
// returns it unchanged without spawning another episode.
func TestDriverResumesDoneCheckpoint(t *testing.T) {
	workdir := t.TempDir()
	failing := fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("spawner must not be called for a done checkpoint")
	}}
	d, spec := driverWith(t, workdir, failing)
	exec, stop, _ := d.Build(spec)

	done, _ := encodeEpisodeCheckpoint(episodeCheckpoint{Done: true, Result: "already finished"})
	gspec := goal.Spec{Objective: "x"}
	out, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, done))
	if err != nil {
		t.Fatalf("Execute on a done checkpoint should not error: %v", err)
	}
	met, reason, _ := stop.Met(context.Background(), gspec, goal.Status{Checkpoint: out})
	if !met || reason != "already finished" {
		t.Errorf("resume did not converge on the prior result: met=%v reason=%q", met, reason)
	}
}

// captureRecorder collects the attested events an episode records, so a test can assert
// what the record would hold. err, when set, fails every Record call.
type captureRecorder struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (c *captureRecorder) Record(_ context.Context, ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, ev)
	return nil
}

func (c *captureRecorder) recorded() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event{}, c.events...)
}

// TestDriverRecordsAttestedEventsVerbatim proves the record keeps the harness's own
// account of its episode, not merely a count of it: every event the tier tally counts is
// handed to the recorder with the CLI's original line, so a reader can see what the
// harness claimed rather than how many claims it made.
func TestDriverRecordsAttestedEventsVerbatim(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"thinking"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	rec := &captureRecorder{}
	d.SetRecorder(rec)

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", goal.Spec{Objective: "think"}, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := rec.recorded()
	// The invariant the sealed record rests on: the declared attested count is the number
	// of attested events the record carries.
	if len(got) != d.Tiers()[TierAttested] {
		t.Fatalf("recorded %d attested event(s), tally declares %d", len(got), d.Tiers()[TierAttested])
	}
	var text Event
	for _, ev := range got {
		if ev.Tier != TierAttested {
			t.Errorf("recorded a %s event; only the harness's own claims belong on this stream", ev.Tier)
		}
		if len(ev.Raw) == 0 {
			t.Errorf("recorded a %s event with no raw line: the harness's account is what makes it worth keeping", ev.Kind)
		}
		if ev.Kind == EventText {
			text = ev
		}
	}
	if want := `{"type":"item.completed","item":{"type":"agent_message","text":"thinking"}}`; string(text.Raw) != want {
		t.Errorf("raw line = %s, want the CLI's line verbatim %s", text.Raw, want)
	}
}

// TestDriverCountsUnrecordedAttestedEvents proves a record that cannot hold the harness's
// account says so. The episode's effects are enforced and recorded at the waist whatever
// the attestation sink does, so a failing sink must not fail the run - but the hole it
// leaves is counted and reported, and the record's declared count will not match the
// events it carries, which verify calls out.
func TestDriverCountsUnrecordedAttestedEvents(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	d.SetRecorder(&captureRecorder{err: errors.New("disk full")})

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", goal.Spec{Objective: "think"}, nil)); err != nil {
		t.Fatalf("an unrecordable attested event failed the episode: %v", err)
	}

	lost, lerr := d.Unrecorded()
	if lost != d.Tiers()[TierAttested] || lost == 0 {
		t.Fatalf("unrecorded = %d, want every one of the %d attested event(s)", lost, d.Tiers()[TierAttested])
	}
	if lerr == nil {
		t.Error("the sink's failure was not reported")
	}
}

// TestDriverRecordsAttestedEventsWithoutReporter proves the record does not depend on a
// live trace being attached: a headless run (no mission reporter) still keeps the
// harness's account, since the record is what a verifier reads afterwards.
func TestDriverRecordsAttestedEventsWithoutReporter(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	spec.Reporter = nil
	rec := &captureRecorder{}
	d.SetRecorder(rec)

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", goal.Spec{Objective: "think"}, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.recorded()) == 0 {
		t.Fatal("a run without a live trace recorded none of the harness's account")
	}
}

// TestDriverRecordsAttestedEventsAfterHalt proves the events leading up to a halt reach
// the record. A halt cancels the episode's context and kills the subprocess, but the
// lines the harness already wrote are still drained and projected; recording them under
// the cancelled context would drop exactly the account of what the harness was doing when
// the run was stopped, which is the account worth reading.
func TestDriverRecordsAttestedEventsAfterHalt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := NewDriver(NewCodex("", nil), nil, t.TempDir())
	rec := &ctxRecorder{}
	d.SetRecorder(rec)
	d.recordAttested(ctx, Event{Kind: EventText, Tier: TierAttested, Raw: json.RawMessage(`{"t":1}`)})

	if lost, _ := d.Unrecorded(); lost != 0 {
		t.Fatalf("a halted episode lost %d attested event(s) to its own cancellation", lost)
	}
	if len(rec.recorded()) != 1 {
		t.Fatal("the event before the halt never reached the record")
	}
}

// ctxRecorder refuses to record under a cancelled context, the way a durable append bound
// to the run's context would.
type ctxRecorder struct{ captureRecorder }

func (c *ctxRecorder) Record(ctx context.Context, ev Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.captureRecorder.Record(ctx, ev)
}

// TestDriverContinuesTheHarnessConversationAcrossTurns is what an interactive session
// rests on. The conversation lives inside the CLI: this side never sees it and could not
// faithfully replay it. So a second turn must hand the CLI back the conversation it
// announced, and put only the new turn to it. If the id were dropped, every turn would
// open a cold session and the user would be talking to an agent with no memory of the
// last thing it said; if the turn text were dropped, the harness would just re-run the
// original objective.
func TestDriverContinuesTheHarnessConversationAcrossTurns(t *testing.T) {
	workdir := t.TempDir()
	var (
		mu    sync.Mutex
		saw   []Episode // one per episode, in order
		turns int
	)
	spawner := scriptSpawner(func(ep Episode, inv Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		mu.Lock()
		saw = append(saw, ep)
		turns++
		n := turns
		mu.Unlock()
		// The CLI announces the conversation it is in. It opens one on the first episode
		// and reports that same one when resumed, exactly as both real CLIs do.
		_, _ = fmt.Fprintln(pw, `{"type":"thread.started","thread_id":"th-99"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
		_ = os.WriteFile(filepath.Join(ep.Workdir, inv.LastMessageFile), fmt.Appendf(nil, "answer %d", n), 0o644)
	})
	d, spec := driverWith(t, workdir, spawner)

	exec, stop, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gspec := goal.Spec{Objective: "the opening line", StopCondition: "done", Grant: []string{"read"}}

	// Turn one: a fresh conversation, driven by the goal's objective.
	ckpt, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil))
	if err != nil {
		t.Fatalf("Execute turn 1: %v", err)
	}
	met, reason, err := stop.Met(context.Background(), gspec, goal.Status{Checkpoint: ckpt})
	if err != nil || !met || reason != "answer 1" {
		t.Fatalf("turn 1 did not converge on its answer: met=%v reason=%q err=%v", met, reason, err)
	}

	// The session puts a second line to the same run.
	status, err := ContinueEpisode(goal.Status{Checkpoint: ckpt}, "and now the second line")
	if err != nil {
		t.Fatalf("ContinueEpisode: %v", err)
	}
	if status.Phase == goal.PhaseConverged {
		t.Fatal("a reopened turn must not still be converged, or nothing would drive it")
	}

	r := goalResource(t, "g1", gspec, status.Checkpoint)
	ckpt2, err := exec.Execute(context.Background(), r)
	if err != nil {
		t.Fatalf("Execute turn 2: %v", err)
	}
	if _, reason, _ := stop.Met(context.Background(), gspec, goal.Status{Checkpoint: ckpt2}); reason != "answer 2" {
		t.Errorf("turn 2 returned %q, want its own answer", reason)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(saw) != 2 {
		t.Fatalf("ran %d episodes, want 2", len(saw))
	}
	// Turn one opened the conversation; turn two continued the one the CLI named.
	if saw[0].Session != "" {
		t.Errorf("the first episode resumed something: %q", saw[0].Session)
	}
	if saw[1].Session != "th-99" {
		t.Errorf("the second episode did not continue the CLI's conversation: %q", saw[1].Session)
	}
	// Turn two puts the new line, not the original objective, to a harness that already
	// holds the context of turn one.
	if !strings.Contains(saw[0].Input, "the opening line") {
		t.Errorf("the first episode's input was %q, want the objective", saw[0].Input)
	}
	if saw[1].Input != "and now the second line" {
		t.Errorf("the second episode's input was %q, want the new turn", saw[1].Input)
	}
}
