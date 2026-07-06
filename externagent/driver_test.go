package externagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

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

// TestDriverRunsEpisodeThroughWaist drives Build -> Execute end to end: the episode
// writes to the workspace through the bridge (governed by the goal's grant), the
// checkpoint records completion and the final message, and the stop evaluator
// converges.
func TestDriverRunsEpisodeThroughWaist(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, inv Invocation, pw *io.PipeWriter) {
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

// TestDriverGrantDeniesUngrantedTool proves the bridge still governs under the
// driver: a goal whose grant omits write cannot write through the bridge even though
// the episode tried, and the episode still completes.
func TestDriverGrantDeniesUngrantedTool(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
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
