package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
)

var errBrokenQueue = errors.New("queue is broken")

// brokenLeases fails every call that touches another claimant's lease.
type brokenLeases struct{ jobs.Queue }

func (brokenLeases) Recover(context.Context) (int, error) { return 0, errBrokenQueue }

func (brokenLeases) ReleasePayload(context.Context, string, []byte) (int, error) {
	return 0, errBrokenQueue
}

// A lease that cannot be released or recovered is reported, never skipped.
func TestRuntimeReportsAQueueThatCannotFreeLeases(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := goal.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	cfg := Config{Executor: stubExec{}, Stop: stubStop{}, Store: store, Jobs: brokenLeases{jobs.NewMemory()}}

	every, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := every.Start(ctx); !errors.Is(err, errBrokenQueue) {
		t.Errorf("Start = %v, want the recover failure", err)
	}

	cfg.DriveSubmittedOnly = true
	own, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(goal.Spec{Objective: "o", StopCondition: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, resource.Resource{
		APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "stored", Spec: raw,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := own.Resume(ctx, "stored"); !errors.Is(err, errBrokenQueue) {
		t.Errorf("Resume = %v, want the release failure", err)
	}
}
