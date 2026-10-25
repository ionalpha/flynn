package instance_test

import (
	"context"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

func TestReachableCondition(t *testing.T) {
	beat := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	window := instance.DefaultStaleAfter
	mk := func(state instance.State, updated time.Time) resource.Resource {
		st, err := resource.EncodeStatus(instance.Status{State: state})
		if err != nil {
			t.Fatal(err)
		}
		return resource.Resource{Status: st, Envelope: resource.Envelope{UpdatedAt: updated}}
	}
	skew := instance.MaxClockSkew
	tests := []struct {
		name        string
		r           resource.Resource
		now         time.Time
		wantStatus  string
		wantReason  string
		unreachable bool
	}{
		{"fresh", mk(instance.StateWorking, beat), beat.Add(time.Second), "True", instance.ReasonHeartbeatFresh, false},
		{"exactly at window", mk(instance.StateIdle, beat), beat.Add(window), "True", instance.ReasonHeartbeatFresh, false},
		{"stale", mk(instance.StateWorking, beat), beat.Add(window + time.Second), "False", instance.ReasonHeartbeatStale, true},
		{"never written", mk(instance.StateIdle, time.Time{}), beat, "False", instance.ReasonHeartbeatStale, true},
		{"done and old", mk(instance.StateDone, beat), beat.Add(time.Hour), "False", instance.ReasonStopped, false},
		{"future within skew", mk(instance.StateWorking, beat.Add(skew)), beat, "True", instance.ReasonHeartbeatFresh, false},
		{"future beyond skew", mk(instance.StateWorking, beat.Add(skew+time.Second)), beat, "False", instance.ReasonHeartbeatStale, true},
		{"far future never ages", mk(instance.StateWorking, beat.AddDate(100, 0, 0)), beat.AddDate(50, 0, 0), "False", instance.ReasonHeartbeatStale, true},
		{"done and fresh", mk(instance.StateDone, beat), beat, "True", instance.ReasonHeartbeatFresh, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := instance.ReachableCondition(tc.r, tc.now, window)
			if c.Type != instance.ConditionReachable || c.Status != tc.wantStatus || c.Reason != tc.wantReason {
				t.Fatalf("condition = %+v, want status %s reason %s", c, tc.wantStatus, tc.wantReason)
			}
			if !c.LastTransitionTime.Equal(tc.r.UpdatedAt) {
				t.Fatalf("LastTransitionTime = %v, want heartbeat %v", c.LastTransitionTime, tc.r.UpdatedAt)
			}
			if got := instance.Unreachable(tc.r, tc.now, window); got != tc.unreachable {
				t.Fatalf("Unreachable = %v, want %v", got, tc.unreachable)
			}
		})
	}
}

func TestUnreachableDisabledByNonPositiveWindow(t *testing.T) {
	r := resource.Resource{Envelope: resource.Envelope{UpdatedAt: time.Unix(0, 0)}}
	if instance.Unreachable(r, time.Unix(1000, 0), 0) {
		t.Fatal("a zero window must disable the check")
	}
}

// TestEmbedderReadsHeartbeat is the embedding contract: a host registers the kind
// on its own store, a heartbeat writer (what flynn serve runs) writes a record,
// and the host reads state, runs, and reachability from it using only the
// exported API.
func TestEmbedderReadsHeartbeat(t *testing.T) {
	reg := resource.NewRegistry()
	if err := instance.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clk := clock.NewManual(time.Unix(0, 0).UTC())
	hb := instance.NewHeartbeat(store, resource.Scope{}, "runner-1",
		instance.Spec{Host: "box-1", Version: "v0.1.0"},
		func(context.Context) (instance.State, []string) { return instance.StateWorking, []string{"run-1"} },
		clk)
	done := make(chan error, 1)
	go func() { done <- hb.Run(ctx) }()

	var r resource.Resource
	for attempt := 0; ; attempt++ {
		got, err := store.Get(ctx, instance.Kind, resource.Scope{}, "runner-1")
		if err == nil {
			if st, derr := instance.DecodeStatus(got); derr == nil && st.State == instance.StateWorking {
				r = got
				break
			}
		}
		if attempt > 5000 {
			t.Fatal("heartbeat never wrote a Working record")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("heartbeat run: %v", err)
	}

	st, err := instance.DecodeStatus(r)
	if err != nil || len(st.Runs) != 1 || st.Runs[0] != "run-1" {
		t.Fatalf("status = %+v, err %v", st, err)
	}
	spec, err := instance.DecodeSpec(r)
	if err != nil || spec.Host != "box-1" {
		t.Fatalf("spec = %+v, err %v", spec, err)
	}
	now := r.UpdatedAt.Add(time.Second)
	if c := instance.ReachableCondition(r, now, instance.DefaultStaleAfter); c.Status != "True" {
		t.Fatalf("fresh heartbeat condition = %+v", c)
	}
	later := r.UpdatedAt.Add(2 * instance.DefaultStaleAfter)
	if !instance.Unreachable(r, later, instance.DefaultStaleAfter) {
		t.Fatal("an aged Working record must be unreachable")
	}
}

// TestIdentityStableAcrossWrites pins the store-owned identity the package
// documents: re-registering and status writes never change the record's ID or
// creation time, and a status write leaves the spec alone.
func TestIdentityStableAcrossWrites(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := instance.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	store := resource.NewMemory(reg)
	first, err := instance.Register(ctx, store, resource.Scope{}, "runner-1", instance.Spec{Host: "box-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = instance.SetStatus(ctx, store, resource.Scope{}, "runner-1", instance.StateWorking, nil); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Register(ctx, store, resource.Scope{}, "runner-1", instance.Spec{Host: "box-1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("identity changed: %s/%v -> %s/%v", first.ID, first.CreatedAt, again.ID, again.CreatedAt)
	}
}
