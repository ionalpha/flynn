package governed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// delegatingModel has the root goal spawn one child. The child answers only when
// finish is set; otherwise its call stays open until the process is stopped.
type delegatingModel struct {
	finish  bool
	started chan struct{}
	once    sync.Once
}

func (m *delegatingModel) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	objective, _, _ := strings.Cut(req.Messages[0].Blocks[0].Text, "\n")
	say := func(text string) llm.Response {
		return llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Kind: llm.KindText, Text: text}}},
			StopReason: llm.StopEndTurn,
		}
	}
	if objective == "child work" {
		if !m.finish {
			m.once.Do(func() { close(m.started) })
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}
		return say("child done"), nil
	}
	if req.Messages[len(req.Messages)-1].Blocks[0].Kind == llm.KindToolResult {
		return say("root done"), nil
	}
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "s", Name: "spawn", Input: json.RawMessage(`{"objective":"child work"}`)}},
		}},
		StopReason: llm.StopToolUse,
	}, nil
}

func assembleFanout(t *testing.T, model llm.Model, b Backends, dir string) *Run {
	t.Helper()
	run, err := Assemble(context.Background(), model, b, WithWorkDir(dir), WithFanout(nil))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}

// parkedFanout runs a root goal until its child is in flight, stops the process,
// and returns the root's name.
func parkedFanout(ctx context.Context, t *testing.T, db, dir string) string {
	t.Helper()
	model := &delegatingModel{started: make(chan struct{})}
	run := assembleFanout(t, model, processBackends(t, db), dir)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = run.Runtime().Start(runCtx); close(done) }()
	defer func() { stop(); <-done }()

	key, err := run.Submit(ctx, goal.Spec{Objective: "root work"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-ctx.Done():
		t.Fatal("the child never started")
	}
	return key.Name
}

// forgetConfinement rewrites every stored goal as one written before goals
// recorded their working directory and grant.
func forgetConfinement(ctx context.Context, t *testing.T, store resource.Store) {
	t.Helper()
	goals, err := store.List(ctx, goal.Kind, resource.Scope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range goals {
		spec, err := goal.DecodeSpec(r)
		if err != nil {
			t.Fatal(err)
		}
		spec.Confinement = nil
		if r.Spec, err = json.Marshal(spec); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// resumeAndWait resumes name in a new process and waits for it to converge.
func resumeAndWait(ctx context.Context, t *testing.T, db, dir, name string, adopt bool) {
	t.Helper()
	run := assembleFanout(t, &delegatingModel{finish: true}, processBackends(t, db), dir)
	startRuntime(t, run)
	if adopt {
		if recorded, err := run.Adopt(ctx, name); err != nil || !recorded {
			t.Fatalf("Adopt = %v, %v, want the record written", recorded, err)
		}
	}
	if err := run.Resume(ctx, name); err != nil {
		t.Fatal(err)
	}
	text, err := run.Session().Wait(ctx)
	if err != nil || text != "root done" {
		t.Fatalf("resumed fan-out = %q, %v", text, err)
	}
}

// A child an earlier process spawned is run by the process that resumes its parent.
func TestResumingAFanoutRunsTheChildrenAlreadySpawned(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "flynn.db")
	dir := t.TempDir()
	resumeAndWait(ctx, t, db, dir, parkedFanout(ctx, t, db, dir), false)
}

// An explicit resume of a run stored without a record covers its children too.
func TestAnAdoptedFanoutIncludesItsChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "flynn.db")
	dir := t.TempDir()
	name := parkedFanout(ctx, t, db, dir)
	forgetConfinement(ctx, t, processBackends(t, db).Resources)
	resumeAndWait(ctx, t, db, dir, name, true)
}

// Only Adopt gives a goal a record: resuming or claiming one without it refuses,
// and Adopt never replaces a record a goal already has.
func TestOnlyAnExplicitAdoptionRunsAGoalStoredWithoutARecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	back := processBackends(t, filepath.Join(t.TempDir(), "flynn.db"))
	dir := t.TempDir()
	model := &writerModel{}
	run := assembleIn(t, model, back, dir)
	key, err := run.Submit(ctx, goal.Spec{Objective: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	forgetConfinement(ctx, t, back.Resources)

	if _, err := run.Runtime().Resume(ctx, key.Name); err == nil {
		t.Fatal("Resume accepted a goal with no record")
	}
	startRuntime(t, run)
	if _, err := run.Session().Wait(ctx); err == nil {
		t.Fatal("a claimed goal with no record converged")
	}
	if seen := model.objectives(); len(seen) != 0 {
		t.Fatalf("ran %q", seen)
	}

	if recorded, err := run.Adopt(ctx, key.Name); err != nil || !recorded {
		t.Fatalf("Adopt = %v, %v, want the record written", recorded, err)
	}
	elsewhere := assembleIn(t, &writerModel{}, back, t.TempDir())
	if recorded, err := elsewhere.Adopt(ctx, key.Name); err != nil || recorded {
		t.Fatalf("second Adopt = %v, %v, want the first record kept", recorded, err)
	}
	if _, err := elsewhere.Runtime().Resume(ctx, key.Name); err == nil {
		t.Fatal("another directory resumed an adopted goal")
	}
	if _, err := run.Adopt(ctx, "goal-missing"); err == nil {
		t.Fatal("Adopt of a missing goal succeeded")
	}
}
