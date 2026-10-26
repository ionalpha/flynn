package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

var psBase = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

func psInstance(t *testing.T, name string, state instance.State, host string, runs []string, heartbeat time.Time) resource.Resource {
	t.Helper()
	spec, err := json.Marshal(instance.Spec{Host: host, Version: "v1"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	status, err := json.Marshal(instance.Status{State: state, Runs: runs})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	return resource.Resource{
		Kind: instance.Kind, Name: name, Spec: spec, Status: status,
		Envelope: resource.Envelope{UpdatedAt: heartbeat},
	}
}

func TestInstanceStatusTable(t *testing.T) {
	fresh := psBase.Add(-10 * time.Second)
	stale := psBase.Add(-2 * instance.DefaultStaleAfter)
	rows := instanceStatusTable([]resource.Resource{
		psInstance(t, "alive", instance.StateWorking, "h1", []string{"r1", "r2"}, fresh),
		psInstance(t, "crashed", instance.StateWorking, "h2", []string{"r3"}, stale),
		psInstance(t, "finished", instance.StateDone, "h3", nil, stale),
	}, psBase).Rows

	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// alive: fresh working stays Working, 2 runs, no stale marker.
	if got := rows[0].Cells; got[3] != "Working" || got[4] != "2" || got[5] != "10s" {
		t.Fatalf("alive row cells = %v", got)
	}
	// crashed: stale working downgrades to Unknown, heartbeat flagged stale.
	if got := rows[1].Cells; got[3] != "Unknown" || got[5] != "3m (stale)" {
		t.Fatalf("crashed row cells = %v", got)
	}
	// finished: Done survives a stale heartbeat (clean shutdown).
	if got := rows[2].Cells; got[3] != "Done" {
		t.Fatalf("finished row cells = %v", got)
	}
}

func TestRunStatusTable(t *testing.T) {
	spec, _ := json.Marshal(goal.Spec{Objective: "ship the release", StopCondition: "done", MaxSteps: 5})
	status, _ := json.Marshal(goal.Status{Phase: goal.PhaseRunning, Steps: 3})
	rows := runStatusTable([]resource.Resource{{
		Kind: goal.Kind, Name: "g-1", Spec: spec, Status: status,
	}}).Rows
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	c := rows[0].Cells
	if c[0] != "g-1" || c[1] != "Running" || c[2] != "3" || c[3] != "ship the release" {
		t.Fatalf("run row cells = %v", c)
	}
}

func TestHeartbeatCell(t *testing.T) {
	never := resource.Resource{}
	if got := heartbeatCell(never, psBase); got != "never" {
		t.Fatalf("zero heartbeat cell = %q, want never", got)
	}
	freshR := resource.Resource{Envelope: resource.Envelope{UpdatedAt: psBase.Add(-5 * time.Second)}}
	if got := heartbeatCell(freshR, psBase); got != "5s" {
		t.Fatalf("fresh heartbeat cell = %q, want 5s", got)
	}
	staleR := resource.Resource{Envelope: resource.Envelope{UpdatedAt: psBase.Add(-5 * time.Minute)}}
	if got := heartbeatCell(staleR, psBase); got != "5m (stale)" {
		t.Fatalf("stale heartbeat cell = %q, want 5m (stale)", got)
	}
}

func TestCompactAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, c := range cases {
		if got := compactAge(c.d); got != c.want {
			t.Fatalf("compactAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
