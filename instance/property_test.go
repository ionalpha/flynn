package instance_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

// TestProp_RegisterStatusRoundTrip checks the kind's two invariants across
// arbitrary specs and state transitions: a status write never disturbs the spec,
// and re-registering (as a restart does) updates the spec while preserving the live
// status.
func TestProp_RegisterStatusRoundTrip(t *testing.T) {
	states := []instance.State{
		instance.StateIdle, instance.StateWorking, instance.StateBlocked,
		instance.StateDone, instance.StateUnknown,
	}

	rapid.Check(t, func(rt *rapid.T) {
		reg := resource.NewRegistry()
		if err := instance.RegisterKind(reg); err != nil {
			rt.Fatalf("register kind: %v", err)
		}
		store := resource.NewMemory(reg)
		ctx := context.Background()

		id := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`).Draw(rt, "id")
		host := rapid.StringMatching(`[a-z0-9.-]{0,20}`).Draw(rt, "host")
		version := rapid.StringMatching(`[a-z0-9.+-]{0,12}`).Draw(rt, "version")
		caps := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,8}`), 0, 5).Draw(rt, "caps")

		r0, err := instance.Register(ctx, store, resource.Scope{}, id,
			instance.Spec{Host: host, Version: version, Capabilities: caps})
		if err != nil {
			rt.Fatalf("register: %v", err)
		}
		spec0, err := instance.DecodeSpec(r0)
		if err != nil {
			rt.Fatalf("decode spec: %v", err)
		}
		if spec0.Host != host || spec0.Version != version || len(spec0.Capabilities) != len(caps) {
			rt.Fatalf("spec round-trip mismatch: %+v", spec0)
		}
		if st0, _ := instance.DecodeStatus(r0); st0.State != instance.StateIdle {
			rt.Fatalf("new instance state = %q, want Idle", st0.State)
		}

		state := states[rapid.IntRange(0, len(states)-1).Draw(rt, "state")]
		runs := rapid.SliceOfN(rapid.StringMatching(`run-[0-9]{1,4}`), 0, 4).Draw(rt, "runs")

		r1, err := instance.SetStatus(ctx, store, resource.Scope{}, id, state, runs)
		if err != nil {
			rt.Fatalf("set status: %v", err)
		}
		if spec1, _ := instance.DecodeSpec(r1); spec1.Host != host || spec1.Version != version {
			rt.Fatalf("SetStatus altered the spec: %+v", spec1)
		}
		if st1, _ := instance.DecodeStatus(r1); st1.State != state || len(st1.Runs) != len(runs) {
			rt.Fatalf("status round-trip mismatch: %+v", st1)
		}

		// A restart re-registers with a possibly new version; the live status survives.
		newVersion := version + "-next"
		r2, err := instance.Register(ctx, store, resource.Scope{}, id,
			instance.Spec{Host: host, Version: newVersion})
		if err != nil {
			rt.Fatalf("re-register: %v", err)
		}
		if spec2, _ := instance.DecodeSpec(r2); spec2.Version != newVersion {
			rt.Fatalf("re-register did not update version: %q", spec2.Version)
		}
		if st2, _ := instance.DecodeStatus(r2); st2.State != state || len(st2.Runs) != len(runs) {
			rt.Fatalf("re-register lost the live status: %+v", st2)
		}
	})
}
