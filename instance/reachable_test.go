package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
		{"forged done does not hide a stale runner", mk(instance.StateDone, beat), beat.Add(time.Hour), "False", instance.ReasonHeartbeatStale, true},
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

func TestNonPositiveWindowStillMarksStaleUnreachable(t *testing.T) {
	r := resource.Resource{Envelope: resource.Envelope{UpdatedAt: time.Unix(0, 0)}}
	now := time.Unix(0, 0).Add(time.Hour)
	for _, w := range []time.Duration{0, -time.Second} {
		if !instance.Unreachable(r, now, w) {
			t.Fatalf("window %v: a stale runner must be unreachable", w)
		}
		if c := instance.ReachableCondition(r, now, w); c.Status != "False" {
			t.Fatalf("window %v: condition = %+v, want False", w, c)
		}
	}
	fresh := resource.Resource{Envelope: resource.Envelope{UpdatedAt: now}}
	if instance.Unreachable(fresh, now, 0) {
		t.Fatal("a fresh runner must stay reachable under the default window")
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

func newCapStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := instance.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

func TestInstanceFieldLengthCapped(t *testing.T) {
	ctx := context.Background()
	store := newCapStore(t)
	long := strings.Repeat("x", instance.MaxFieldLen+1)
	if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: "h"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, instance.Kind, resource.Scope{}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]instance.Spec{
		"host":    {Host: long},
		"version": {Version: long},
		"cap":     {Capabilities: []string{long}},
		"caps":    {Capabilities: make([]string, instance.MaxListLen+1)},
	}
	for name, spec := range specs {
		if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", spec); !errors.Is(err, instance.ErrTooLarge) {
			t.Fatalf("%s: err = %v, want ErrTooLarge", name, err)
		}
	}
	after, err := store.Get(ctx, instance.Kind, resource.Scope{}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || string(after.Spec) != string(before.Spec) || string(after.Status) != string(before.Status) {
		t.Fatal("a refused write changed the stored record")
	}
}

// TestInstanceRecordSizeCapped fills every list with entries that each fit their
// own cap, so only the record-wide cap can refuse the write.
func TestInstanceRecordSizeCapped(t *testing.T) {
	ctx := context.Background()
	store := newCapStore(t)
	entry := strings.Repeat("x", instance.MaxFieldLen)
	caps := make([]string, instance.MaxListLen)
	runs := make([]string, instance.MaxListLen)
	for i := range caps {
		caps[i], runs[i] = entry, entry
	}
	if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{}); err != nil {
		t.Fatal(err)
	}
	if 2*instance.MaxListLen*instance.MaxFieldLen <= instance.MaxRecordBytes {
		t.Fatal("per-field caps already imply the record cap; the record cap is dead code")
	}
	if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateWorking, runs); err != nil {
		t.Fatalf("a status at its own caps must fit: %v", err)
	}
	if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Capabilities: caps}); !errors.Is(err, instance.ErrTooLarge) {
		t.Fatalf("a spec that overflows the record around the stored status: err = %v", err)
	}
	if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateIdle, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Capabilities: caps}); err != nil {
		t.Fatalf("a spec at its own caps must fit: %v", err)
	}
	if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateWorking, runs); !errors.Is(err, instance.ErrTooLarge) {
		t.Fatalf("a status that overflows the record around the stored spec: err = %v", err)
	}
}

// TestInstanceHasNoGoalTextOrSecretFields fails on any spec or status field not in
// the reviewed list, so adding a free-text field (goal text, a prompt, a token, an
// address, a label) forces a decision about its cap and rendering here.
func TestInstanceHasNoGoalTextOrSecretFields(t *testing.T) {
	reviewed := map[string]bool{
		"Spec.Host": true, "Spec.Version": true, "Spec.Capabilities": true,
		"Status.State": true, "Status.Runs": true, "Status.ObservedGeneration": true, "Status.OmittedRuns": true,
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(instance.Spec{}), reflect.TypeOf(instance.Status{})} {
		for i := range typ.NumField() {
			name := typ.Name() + "." + typ.Field(i).Name
			if !reviewed[name] {
				t.Errorf("unreviewed field %s: cap it, check it carries no goal text or secret, then list it here", name)
			}
		}
	}
}

func TestStatusWriteCannotChangeSpec(t *testing.T) {
	ctx := context.Background()
	store := newCapStore(t)
	first, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: "h", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	afterStatus, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateBlocked, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if string(afterStatus.Spec) != string(first.Spec) {
		t.Fatalf("status write changed spec: %s -> %s", first.Spec, afterStatus.Spec)
	}
	afterSpec, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: "h", Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSpec.Status) != string(afterStatus.Status) {
		t.Fatalf("spec write changed status: %s -> %s", afterStatus.Status, afterSpec.Status)
	}
}

// TestOverReportedRunsAreTruncatedNotRefused pins that a Reporter naming too many
// or too long run ids still lands its beat (so the runner stays reachable) and
// the status says how many were dropped.
func TestOverReportedRunsAreTruncatedNotRefused(t *testing.T) {
	ctx := context.Background()
	store := newCapStore(t)
	if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{}); err != nil {
		t.Fatal(err)
	}
	runs := make([]string, instance.MaxListLen+5)
	for i := range runs {
		runs[i] = "run"
	}
	runs[0] = strings.Repeat("x", instance.MaxFieldLen+1)
	r, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateWorking, runs)
	if err != nil {
		t.Fatalf("an over-reporting beat must still be written: %v", err)
	}
	st, err := instance.DecodeStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Runs) != instance.MaxListLen || st.OmittedRuns != 5 {
		t.Fatalf("runs = %d, omitted = %d, want %d and 5", len(st.Runs), st.OmittedRuns, instance.MaxListLen)
	}
}

// corruptRead serves every stored record with one half replaced by JSON that does not
// decode into it. The store refuses such a record on write, so a corrupted half can only
// arrive from beneath it (a host's own store, a row edited by hand), and this stands in
// for that.
type corruptRead struct {
	resource.Store
	half string
}

func (c corruptRead) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	r, err := c.Store.Get(ctx, kind, scope, name)
	if err != nil {
		return r, err
	}
	if c.half == "spec" {
		r.Spec = json.RawMessage(`[]`)
	} else {
		r.Status = json.RawMessage(`[]`)
	}
	return r, nil
}

// TestUndecodableStoredHalfStillCapsTheWrite: Register and SetStatus size the merged
// record against the half already stored. A stored half that does not decode counts as
// empty, so the write still goes through the caps instead of failing on the corruption or
// skipping the check: an oversized write is refused with [instance.ErrTooLarge].
func TestUndecodableStoredHalfStillCapsTheWrite(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", instance.MaxFieldLen+1)
	corrupt := func(t *testing.T, half string) resource.Store {
		t.Helper()
		store := newCapStore(t)
		if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: "h"}); err != nil {
			t.Fatal(err)
		}
		return corruptRead{Store: store, half: half}
	}

	t.Run("status", func(t *testing.T) {
		store := corrupt(t, "status")
		if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: long}); !errors.Is(err, instance.ErrTooLarge) {
			t.Fatalf("oversized spec over a corrupt status: err = %v, want ErrTooLarge", err)
		}
		if _, err := instance.Register(ctx, store, resource.Scope{}, "r1", instance.Spec{Host: "h2"}); err != nil {
			t.Fatalf("spec within its caps over a corrupt status: %v", err)
		}
	})
	t.Run("spec", func(t *testing.T) {
		store := corrupt(t, "spec")
		if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.State(long), nil); !errors.Is(err, instance.ErrTooLarge) {
			t.Fatalf("oversized status over a corrupt spec: err = %v, want ErrTooLarge", err)
		}
		// SetStatus writes the stored spec back unchanged, so the store's own schema
		// check is what refuses this one: the caps pass and the corruption is not
		// laundered into a fresh write.
		if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "r1", instance.StateWorking, []string{"run-1"}); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("status within its caps over a corrupt spec: err = %v, want the store's ErrInvalid", err)
		}
	})
}
