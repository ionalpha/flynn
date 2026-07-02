package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/reconcile"
)

// fakeDriver records the calls the supervisor makes and returns scripted results, so a
// reconcile can be observed without a provider.
type fakeDriver struct {
	obs      Observation
	obsErr   error
	teardErr error
	observed []Service
	tornDown []Service
}

func (d *fakeDriver) Observe(_ context.Context, svc Service) (Observation, error) {
	d.observed = append(d.observed, svc)
	return d.obs, d.obsErr
}

func (d *fakeDriver) Teardown(_ context.Context, svc Service) error {
	d.tornDown = append(d.tornDown, svc)
	return d.teardErr
}

func keyOf(name string) reconcile.Ref { return reconcile.Ref{Kind: Kind, Name: name} }

// TestSupervisorObservesRunning proves a running service is observed and its status is
// updated from the observation, and the supervisor asks to be re-observed.
func TestSupervisorObservesRunning(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateRunning}, Status{Phase: "deployed"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	drv := &fakeDriver{obs: Observation{Phase: "running", URL: "https://site.pages.dev"}}
	sup := NewSupervisor(s, drv, WithPoll(0))

	res, err := sup.Reconcile(ctx, keyOf("site"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drv.observed) != 1 {
		t.Fatalf("expected one observe, got %d", len(drv.observed))
	}
	if res.RequeueAfter != 0 { // WithPoll(0) disables periodic re-observe
		t.Fatalf("unexpected requeue: %v", res.RequeueAfter)
	}
	got, err := s.Get(ctx, "site")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != "running" || got.Status.ObservedURL != "https://site.pages.dev" {
		t.Fatalf("status not updated from observation: %+v", got.Status)
	}
}

// TestSupervisorNoChurnWhenUnchanged proves an observation that matches the recorded
// status does not write a new resource version, so a healthy service does not churn the
// store on every poll.
func TestSupervisorNoChurnWhenUnchanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateRunning}, Status{Phase: "running", ObservedURL: "https://site.pages.dev"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	before, _ := s.Get(ctx, "site")
	drv := &fakeDriver{obs: Observation{Phase: "running", URL: "https://site.pages.dev"}}
	sup := NewSupervisor(s, drv)

	if _, err := sup.Reconcile(ctx, keyOf("site")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, _ := s.Get(ctx, "site")
	if after.Version != before.Version {
		t.Fatalf("status churned on an unchanged observation: v%d -> v%d", before.Version, after.Version)
	}
}

// TestSupervisorTearsDownStopped proves a service marked stopped is torn down through
// its provider and its record retired, in that order.
func TestSupervisorTearsDownStopped(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateStopped, ExternalID: "dep-1"}, Status{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	drv := &fakeDriver{}
	sup := NewSupervisor(s, drv)

	if _, err := sup.Reconcile(ctx, keyOf("site")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drv.tornDown) != 1 || drv.tornDown[0].Spec.ExternalID != "dep-1" {
		t.Fatalf("expected one teardown of dep-1, got %+v", drv.tornDown)
	}
	if _, err := s.Get(ctx, "site"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the record retired after teardown, got %v", err)
	}
}

// TestSupervisorKeepsRecordOnTeardownError proves a failed teardown leaves the record
// in place so the next reconcile retries, never deleting a workload it could not remove.
func TestSupervisorKeepsRecordOnTeardownError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateStopped}, Status{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	drv := &fakeDriver{teardErr: fault.New(fault.Transient, "boom", "provider down")}
	sup := NewSupervisor(s, drv)

	if _, err := sup.Reconcile(ctx, keyOf("site")); err == nil {
		t.Fatal("expected the teardown error to propagate")
	}
	if _, err := s.Get(ctx, "site"); err != nil {
		t.Fatalf("record must survive a failed teardown: %v", err)
	}
}

// TestSupervisorVanishedServiceSettles proves a reconcile for a service that no longer
// exists settles without touching the driver.
func TestSupervisorVanishedServiceSettles(t *testing.T) {
	s := testStore(t)
	drv := &fakeDriver{}
	sup := NewSupervisor(s, drv)

	res, err := sup.Reconcile(context.Background(), keyOf("ghost"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 || len(drv.observed) != 0 || len(drv.tornDown) != 0 {
		t.Fatalf("a vanished service should settle untouched: %+v", res)
	}
}

// TestSupervisorObserveErrorRetries proves an observe failure is returned so the
// controller retries, and the status is left untouched.
func TestSupervisorObserveErrorRetries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "site", Spec{Provider: "cloudflare", DesiredState: StateRunning}, Status{Phase: "deployed"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	drv := &fakeDriver{obsErr: fault.New(fault.Transient, "boom", "provider down")}
	sup := NewSupervisor(s, drv)

	if _, err := sup.Reconcile(ctx, keyOf("site")); err == nil {
		t.Fatal("expected the observe error to propagate")
	}
	got, _ := s.Get(ctx, "site")
	if got.Status.Phase != "deployed" {
		t.Fatalf("status must be untouched on an observe error: %+v", got.Status)
	}
}
