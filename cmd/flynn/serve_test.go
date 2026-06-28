package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

// goalListStore is a minimal resource.Store that serves a fixed goal list, so the
// reporter's ownership filter and phase mapping can be exercised with full control
// over each goal's OriginInstanceID (which a real Put stamps from the writer).
type goalListStore struct {
	resource.Store
	goals []resource.Resource
	err   error
}

func (g goalListStore) ListAll(context.Context, string, resource.Selector) ([]resource.Resource, error) {
	return g.goals, g.err
}

func goalRes(t *testing.T, name, origin string, phase goal.Phase) resource.Resource {
	t.Helper()
	status, err := json.Marshal(goal.Status{Phase: phase})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	return resource.Resource{
		Kind:     goal.Kind,
		Name:     name,
		Status:   status,
		Envelope: resource.Envelope{OriginInstanceID: origin},
	}
}

func TestInstanceReporter(t *testing.T) {
	const me = "node-a"
	cases := []struct {
		name      string
		goals     []resource.Resource
		err       error
		wantState instance.State
		wantRuns  []string
	}{
		{
			name:      "no goals is idle",
			wantState: instance.StateIdle,
		},
		{
			name: "active goals make it working",
			goals: []resource.Resource{
				goalRes(t, "g-run", me, goal.PhaseRunning),
				goalRes(t, "g-pend", me, goal.PhasePending),
			},
			wantState: instance.StateWorking,
			wantRuns:  []string{"g-run", "g-pend"},
		},
		{
			name: "terminal goals do not count as work",
			goals: []resource.Resource{
				goalRes(t, "g-done", me, goal.PhaseConverged),
				goalRes(t, "g-fail", me, goal.PhaseStalled),
			},
			wantState: instance.StateIdle,
		},
		{
			name: "another instance's run is excluded",
			goals: []resource.Resource{
				goalRes(t, "g-other", "node-b", goal.PhaseRunning),
			},
			wantState: instance.StateIdle,
		},
		{
			name: "blank origin is treated as local",
			goals: []resource.Resource{
				goalRes(t, "g-legacy", "", goal.PhaseRunning),
			},
			wantState: instance.StateWorking,
			wantRuns:  []string{"g-legacy"},
		},
		{
			name:      "a read error reports unknown",
			err:       errors.New("store down"),
			wantState: instance.StateUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := instanceReporter(goalListStore{goals: tc.goals, err: tc.err}, me)
			state, runs := report(context.Background())
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if len(runs) != len(tc.wantRuns) {
				t.Fatalf("runs = %v, want %v", runs, tc.wantRuns)
			}
			for i, r := range tc.wantRuns {
				if runs[i] != r {
					t.Fatalf("runs = %v, want %v", runs, tc.wantRuns)
				}
			}
		})
	}
}
