package instance_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := instance.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

func TestRegisterCreatesIdleInstance(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	r, err := instance.Register(ctx, store, resource.Scope{}, "node-a", instance.Spec{
		Host: "host-1", Version: "v1.2.3", Capabilities: []string{"bash", "fs"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	spec, err := instance.DecodeSpec(r)
	if err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec.Host != "host-1" || spec.Version != "v1.2.3" || len(spec.Capabilities) != 2 {
		t.Fatalf("spec round-trip wrong: %+v", spec)
	}
	st, err := instance.DecodeStatus(r)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.State != instance.StateIdle {
		t.Fatalf("new instance state = %q, want Idle", st.State)
	}
}

func TestReregisterPreservesStatus(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := instance.Register(ctx, store, resource.Scope{}, "node-a", instance.Spec{Version: "v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := instance.SetStatus(ctx, store, resource.Scope{}, "node-a", instance.StateWorking, []string{"run-1"}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	// Re-register (as a restart would) with a new version: status must survive.
	r, err := instance.Register(ctx, store, resource.Scope{}, "node-a", instance.Spec{Version: "v2"})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	spec, _ := instance.DecodeSpec(r)
	if spec.Version != "v2" {
		t.Fatalf("spec version = %q, want v2", spec.Version)
	}
	st, _ := instance.DecodeStatus(r)
	if st.State != instance.StateWorking || len(st.Runs) != 1 || st.Runs[0] != "run-1" {
		t.Fatalf("status not preserved across re-register: %+v", st)
	}
}

func TestSetStatusPreservesSpec(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := instance.Register(ctx, store, resource.Scope{}, "node-a", instance.Spec{Host: "host-1", Version: "v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	r, err := instance.SetStatus(ctx, store, resource.Scope{}, "node-a", instance.StateBlocked, nil)
	if err != nil {
		t.Fatalf("set status: %v", err)
	}
	spec, _ := instance.DecodeSpec(r)
	if spec.Host != "host-1" || spec.Version != "v1" {
		t.Fatalf("spec not preserved by SetStatus: %+v", spec)
	}
	st, _ := instance.DecodeStatus(r)
	if st.State != instance.StateBlocked {
		t.Fatalf("state = %q, want Blocked", st.State)
	}
}

func TestSetStatusUnknownInstanceErrors(t *testing.T) {
	_, err := instance.SetStatus(context.Background(), newStore(t), resource.Scope{}, "ghost", instance.StateIdle, nil)
	if err == nil {
		t.Fatal("SetStatus on an unregistered instance should error")
	}
}
