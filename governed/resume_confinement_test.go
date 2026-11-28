package governed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

// finishedRun runs one goal in dir and returns its id.
func finishedRun(ctx context.Context, t *testing.T, db, dir string) string {
	t.Helper()
	res, err := assembleIn(t, &writerModel{}, processBackends(t, db), dir).Do(ctx, goal.Spec{Objective: "first"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res.RunID
}

func TestResumeRefusesAGoalThisRunIsNotConfinedFor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "flynn.db")
	dir := t.TempDir()
	id := finishedRun(ctx, t, db, dir)

	unrecorded := processBackends(t, db)
	raw, err := json.Marshal(goal.Spec{Objective: "old", StopCondition: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unrecorded.Resources.Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "goal-unrecorded", Spec: raw,
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, id, dir, want string }{
		{"another directory", id, t.TempDir(), "working directory"},
		{"no record", "goal-unrecorded", dir, "records no working directory"},
	} {
		_, err := assembleIn(t, &writerModel{}, processBackends(t, db), c.dir).Runtime().Resume(ctx, c.id)
		if err == nil || !strings.Contains(err.Error(), c.want) || fault.Classify(err) != fault.Terminal {
			t.Errorf("%s: Resume = %v, want a terminal refusal naming %q", c.name, err, c.want)
		}
	}
	if _, err := assembleIn(t, &writerModel{}, processBackends(t, db), dir).Runtime().Resume(ctx, id); err != nil {
		t.Errorf("same directory and grant: Resume = %v", err)
	}
}

// A goal whose record changes under a running process stops before its next step.
func TestAStepIsNotRunOnceItsGoalRecordsAnotherDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	back := processBackends(t, filepath.Join(t.TempDir(), "flynn.db"))
	model := &writerModel{}
	run := assembleIn(t, model, back, t.TempDir())
	key, err := run.Submit(ctx, goal.Spec{Objective: "moved"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := back.Resources.Get(ctx, key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	spec.Confinement.Workdir = t.TempDir()
	if r.Spec, err = json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := back.Resources.Put(ctx, r); err != nil {
		t.Fatal(err)
	}

	startRuntime(t, run)
	if _, err := run.Session().Wait(ctx); err == nil {
		t.Fatal("the goal converged")
	}
	if seen := model.objectives(); len(seen) != 0 {
		t.Fatalf("ran %q", seen)
	}
}

// The holder of a crashed run's step is gone, and resuming it must not wait out the lease.
func TestResumeTakesUpTheStepACrashedProcessLeftLeased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "flynn.db")
	dir := t.TempDir()

	// The first process dispatches its step and claims it, then is never heard from.
	first := processBackends(t, db)
	dead := assembleIn(t, &writerModel{}, first, dir)
	key, err := dead.Submit(ctx, goal.Spec{Objective: "survivor"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := first.Resources.Get(ctx, key.Kind, key.Scope, key.Name)
	if err != nil {
		t.Fatal(err)
	}
	job, err := first.Jobs.Enqueue(ctx, jobs.EnqueueParams{Queue: goal.StepQueue, Kind: goal.StepJobKind, Payload: []byte(r.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if held, err := first.Jobs.Claim(ctx, jobs.ClaimParams{Queue: goal.StepQueue, LeaseFor: int64(time.Hour)}); err != nil || len(held) != 1 {
		t.Fatalf("claim: %v %v", held, err)
	}

	next := assembleIn(t, &writerModel{}, processBackends(t, db), dir)
	startRuntime(t, next)
	if err := next.Resume(ctx, key.Name); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := first.Jobs.Get(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == jobs.StateDone {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the leased step was not taken up: %s", got.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
