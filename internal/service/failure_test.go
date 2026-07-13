package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
)

// errBackend is the failure a broken or unreachable resource backend reports.
var errBackend = errors.New("backend unreachable")

// faultyStore wraps a resource store and fails the selected operations, modelling a
// backend that is down. Anything not overridden delegates to the embedded store.
type faultyStore struct {
	resource.Store
	putErr  error
	getErr  error
	listErr error
	delErr  error
}

func (f faultyStore) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if f.putErr != nil {
		return resource.Resource{}, f.putErr
	}
	return f.Store.Put(ctx, r)
}

func (f faultyStore) Get(ctx context.Context, kind string, scope resource.Scope, name string) (resource.Resource, error) {
	if f.getErr != nil {
		return resource.Resource{}, f.getErr
	}
	return f.Store.Get(ctx, kind, scope, name)
}

func (f faultyStore) List(ctx context.Context, kind string, scope resource.Scope, sel resource.Selector) ([]resource.Resource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.List(ctx, kind, scope, sel)
}

func (f faultyStore) Delete(ctx context.Context, kind string, scope resource.Scope, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	return f.Store.Delete(ctx, kind, scope, name)
}

// corruptStore serves a record whose spec is not a service object, modelling a record
// written by an incompatible version.
type corruptStore struct{ resource.Store }

func badResource() resource.Resource {
	return resource.Resource{APIVersion: GroupVersion, Kind: Kind, Name: "site", Spec: json.RawMessage(`"not-an-object"`)}
}

func (corruptStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return badResource(), nil
}

func (corruptStore) List(context.Context, string, resource.Scope, resource.Selector) ([]resource.Resource, error) {
	return []resource.Resource{badResource()}, nil
}

func memStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return resource.NewMemory(reg)
}

// TestTargetValid proves only the declared deploy targets are accepted, with the empty
// target allowed for a provider that does not classify its output.
func TestTargetValid(t *testing.T) {
	for _, ok := range []Target{"", TargetStaticSite, TargetContainer, TargetVPS} {
		if !ok.Valid() {
			t.Fatalf("%q should be a valid target", ok)
		}
	}
	for _, bad := range []Target{"lambda", "static site", "VPS"} {
		if bad.Valid() {
			t.Fatalf("%q should not be a valid target", bad)
		}
	}
}

// TestPutRequiresNameAndProvider proves an unaddressable record is refused: a service
// with no name cannot be re-found, and one with no provider cannot be supervised.
func TestPutRequiresNameAndProvider(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "", Spec{Provider: "cloudflare"}, Status{}); err == nil {
		t.Fatal("a service with no name must be refused")
	}
	if _, err := s.Put(ctx, "site", Spec{}, Status{}); err == nil {
		t.Fatal("a service with no provider must be refused")
	}
}

// TestDecodeStatusMalformed proves a status that is not a status object is an error,
// not silently read as an empty status.
func TestDecodeStatusMalformed(t *testing.T) {
	st, err := DecodeStatus(resource.Resource{Status: json.RawMessage(`{"phase":"deployed"}`)})
	if err != nil || st.Phase != "deployed" {
		t.Fatalf("status round trip: %+v %v", st, err)
	}
	if _, err := DecodeStatus(resource.Resource{Status: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("a malformed status must be an error")
	}
	if _, err := DecodeSpec(badResource()); err == nil {
		t.Fatal("a malformed spec must be an error")
	}
}

// TestToServiceRejectsCorruptStatus proves a record whose status cannot be decoded is
// rejected, not returned with the status quietly zeroed.
func TestToServiceRejectsCorruptStatus(t *testing.T) {
	r := resource.Resource{
		Name:   "site",
		Spec:   json.RawMessage(`{"provider":"cloudflare"}`),
		Status: json.RawMessage(`[]`),
	}
	if _, err := toService(r); err == nil {
		t.Fatal("a corrupt status must fail the read")
	}
}

// TestReadPathsSurfaceCorruptSpec proves a record that cannot be decoded fails the read
// rather than yielding a zero service the supervisor would then act on.
func TestReadPathsSurfaceCorruptSpec(t *testing.T) {
	s := NewStore(corruptStore{memStore(t)})
	ctx := context.Background()
	if _, err := s.Get(ctx, "site"); err == nil {
		t.Fatal("Get must surface the decode error")
	}
	if _, err := s.List(ctx); err == nil {
		t.Fatal("List must surface the decode error")
	}
}

// TestBackendErrorsPropagate proves a store outage is reported on every operation, and
// is distinguishable from ErrNotFound so a caller retries instead of concluding the
// service is gone.
func TestBackendErrorsPropagate(t *testing.T) {
	ctx := context.Background()

	putBroken := NewStore(faultyStore{Store: memStore(t), putErr: errBackend})
	if _, err := putBroken.Put(ctx, "site", Spec{Provider: "cloudflare"}, Status{}); !errors.Is(err, errBackend) {
		t.Fatalf("Put error = %v", err)
	}

	getBroken := NewStore(faultyStore{Store: memStore(t), getErr: errBackend})
	_, err := getBroken.Get(ctx, "site")
	if !errors.Is(err, errBackend) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get must report the outage, not a missing service: %v", err)
	}

	listBroken := NewStore(faultyStore{Store: memStore(t), listErr: errBackend})
	if _, err := listBroken.List(ctx); !errors.Is(err, errBackend) {
		t.Fatalf("List error = %v", err)
	}

	delBroken := NewStore(faultyStore{Store: memStore(t), delErr: errBackend})
	if err := delBroken.Delete(ctx, "site"); !errors.Is(err, errBackend) {
		t.Fatalf("Delete error = %v", err)
	}
}

// TestDeleteMissingIsNoError proves retiring an already-gone record is idempotent, so a
// teardown that crashed after removing the record can safely run again.
func TestDeleteMissingIsNoError(t *testing.T) {
	if err := testStore(t).Delete(context.Background(), "ghost"); err != nil {
		t.Fatalf("deleting a missing service must be a no-op, got %v", err)
	}
}

// TestSupervisorStoreErrorRetries proves a store read failure is returned so the
// controller retries, rather than being read as a vanished service and settled.
func TestSupervisorStoreErrorRetries(t *testing.T) {
	s := NewStore(faultyStore{Store: memStore(t), getErr: errBackend})
	drv := &fakeDriver{}
	sup := NewSupervisor(s, drv)

	if _, err := sup.Reconcile(context.Background(), keyOf("site")); !errors.Is(err, errBackend) {
		t.Fatalf("a store read error must propagate, got %v", err)
	}
	if len(drv.observed) != 0 || len(drv.tornDown) != 0 {
		t.Fatal("the driver must not be touched when the service could not be read")
	}
}

// TestSupervisorStatusWriteErrorRetries proves a failed status write is returned, so
// the observation is re-taken instead of being silently lost.
func TestSupervisorStatusWriteErrorRetries(t *testing.T) {
	ctx := context.Background()
	backing := memStore(t)
	if _, err := NewStore(backing).Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateRunning}, Status{Phase: "deployed"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	broken := NewStore(faultyStore{Store: backing, putErr: errBackend})
	sup := NewSupervisor(broken, &fakeDriver{obs: Observation{Phase: "running"}})

	if _, err := sup.Reconcile(ctx, keyOf("site")); !errors.Is(err, errBackend) {
		t.Fatalf("a failed status write must propagate, got %v", err)
	}
}

// TestSupervisorRecordDeleteErrorRetries proves that when the remote workload is gone
// but the record cannot be retired, the error is returned so the reconcile runs again;
// the driver's Teardown is idempotent for exactly this case.
func TestSupervisorRecordDeleteErrorRetries(t *testing.T) {
	ctx := context.Background()
	backing := memStore(t)
	if _, err := NewStore(backing).Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateStopped}, Status{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	broken := NewStore(faultyStore{Store: backing, delErr: errBackend})
	drv := &fakeDriver{}
	sup := NewSupervisor(broken, drv)

	if _, err := sup.Reconcile(ctx, keyOf("site")); !errors.Is(err, errBackend) {
		t.Fatalf("a failed record delete must propagate, got %v", err)
	}
	if len(drv.tornDown) != 1 {
		t.Fatalf("the workload should still have been torn down once, got %d", len(drv.tornDown))
	}
}

// TestSupervisorUnsetDesiredStateIsSupervised proves an unset desired state means
// "keep it up": the workload is observed, not abandoned.
func TestSupervisorUnsetDesiredStateIsSupervised(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare"}, Status{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	drv := &fakeDriver{obs: Observation{Phase: "running"}}
	sup := NewSupervisor(s, drv)

	res, err := sup.Reconcile(ctx, keyOf("site"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drv.observed) != 1 {
		t.Fatalf("an unset desired state must still be observed, got %d observes", len(drv.observed))
	}
	if res.RequeueAfter != DefaultPoll {
		t.Fatalf("RequeueAfter = %v, want the default poll", res.RequeueAfter)
	}
}

// TestWithClockOverride proves an injected time source is taken and a nil one leaves
// the default in place, so a caller cannot accidentally install a nil clock.
func TestWithClockOverride(t *testing.T) {
	manual := clock.NewManual(time.Unix(0, 0).UTC())
	sup := NewSupervisor(testStore(t), &fakeDriver{}, WithClock(manual))
	if sup.clk != clock.Timing(manual) {
		t.Fatalf("WithClock must install the injected clock, got %T", sup.clk)
	}
	def := NewSupervisor(testStore(t), &fakeDriver{}, WithClock(nil))
	if _, ok := def.clk.(clock.System); !ok {
		t.Fatalf("WithClock(nil) must keep the system clock, got %T", def.clk)
	}
}
