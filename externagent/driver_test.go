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
