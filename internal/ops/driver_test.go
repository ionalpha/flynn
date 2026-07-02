package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/resource"
)

// driverFixture builds a resource store holding one hosting extension and a loader
// wired with the ops handler, the pieces a Driver supervises through. The extension's
// ops surface carries the operations the test needs; flows are network-free return
// flows so a driver call resolves and runs without a transport.
func driverFixture(t *testing.T, provider string, ops []opSpec) (*Driver, resource.Store) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := extension.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)

	spec := extension.Spec{
		Provider: provider,
		BaseURL:  "https://api.example",
		Auth:     extension.AuthSpec{Type: "none"},
		Surfaces: map[string]json.RawMessage{
			extension.SurfaceOps: opsBlock(t, ops),
		},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if _, err := store.Put(context.Background(), resource.Resource{
		APIVersion: extension.GroupVersion, Kind: extension.Kind, Name: provider, Spec: raw,
	}); err != nil {
		t.Fatalf("put extension: %v", err)
	}

	ereg := extension.NewRegistry()
	if err := RegisterWith(ereg); err != nil {
		t.Fatalf("register ops handler: %v", err)
	}
	return NewDriver(store, extension.NewLoader(ereg)), store
}

// opSpec is a tiny test description of a hosting operation: its name and the return
// flow body it yields.
type opSpec struct {
	name   string
	result string // JSON object the flow returns
}

func opsBlock(t *testing.T, ops []opSpec) json.RawMessage {
	t.Helper()
	var sp Spec
	sp.Targets = []service.Target{service.TargetStaticSite}
	for _, o := range ops {
		flow := json.RawMessage(`{"steps":[{"op":"return","return":{"value":` + o.result + `}}]}`)
		sp.Operations = append(sp.Operations, integrations.Operation{Name: o.name, Flow: flow})
	}
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("marshal ops block: %v", err)
	}
	return b
}

// TestDriverObserveParsesStatus proves Observe loads the provider, runs its status
// operation, and reads the workload's phase and URL from the result.
func TestDriverObserveParsesStatus(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{
		{name: OpDeploy, result: `{"url":"x"}`},
		{name: OpStatus, result: `{"phase":"running","url":"https://site.pages.dev"}`},
	})
	svc := service.Service{Name: "site", Spec: service.Spec{Provider: "cloudflare"}}

	obs, err := drv.Observe(context.Background(), svc)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs.Phase != "running" || obs.URL != "https://site.pages.dev" {
		t.Fatalf("observation: %+v", obs)
	}
}

// TestDriverObserveEnvelopeReachesFlow proves the service's external id and its opaque
// Address keys are flattened into the operation input, so a provider operation reads
// them as ordinary config (here the status flow echoes config.project_name back).
func TestDriverObserveEnvelopeReachesFlow(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{
		{name: OpDeploy, result: `{"url":"x"}`},
		{name: OpStatus, result: `{"phase":"running","url":"{{config.project_name}}"}`},
	})
	svc := service.Service{Name: "site", Spec: service.Spec{
		Provider:   "cloudflare",
		ExternalID: "dep-1",
		Address:    map[string]string{"project_name": "my-project", "account_id": "acc-1"},
	}}

	obs, err := drv.Observe(context.Background(), svc)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs.URL != "my-project" {
		t.Fatalf("address key did not reach the flow as config: %+v", obs)
	}
}

// TestDriverObserveNoStatusOpIsUnknown proves a provider without a status operation
// yields an empty observation rather than an error, so the supervisor keeps the last
// known status and re-checks later.
func TestDriverObserveNoStatusOpIsUnknown(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{
		{name: OpDeploy, result: `{"url":"x"}`},
	})
	svc := service.Service{Name: "site", Spec: service.Spec{Provider: "cloudflare"}}

	obs, err := drv.Observe(context.Background(), svc)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs != (service.Observation{}) {
		t.Fatalf("expected an empty observation, got %+v", obs)
	}
}

// TestDriverTeardownRunsOp proves Teardown runs the provider's teardown operation.
func TestDriverTeardownRunsOp(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{
		{name: OpDeploy, result: `{"url":"x"}`},
		{name: OpTeardown, result: `{"deleted":true}`},
	})
	svc := service.Service{Name: "site", Spec: service.Spec{Provider: "cloudflare"}}

	if err := drv.Teardown(context.Background(), svc); err != nil {
		t.Fatalf("teardown: %v", err)
	}
}

// TestDriverTeardownWithoutOpIsTerminal proves that a provider declaring no teardown
// operation fails terminally rather than retrying, so the supervisor does not spin on a
// workload it can never retire automatically.
func TestDriverTeardownWithoutOpIsTerminal(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{
		{name: OpDeploy, result: `{"url":"x"}`},
	})
	svc := service.Service{Name: "site", Spec: service.Spec{Provider: "cloudflare"}}

	err := drv.Teardown(context.Background(), svc)
	if err == nil {
		t.Fatal("expected a provider with no teardown operation to fail")
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("expected a terminal error, got %v (%v)", fault.Classify(err), err)
	}
}

// TestDriverUnknownProvider proves supervising a service whose provider has no
// extension fails terminally with a clear message.
func TestDriverUnknownProvider(t *testing.T) {
	drv, _ := driverFixture(t, "cloudflare", []opSpec{{name: OpDeploy, result: `{"url":"x"}`}})
	svc := service.Service{Name: "site", Spec: service.Spec{Provider: "fly"}}

	if _, err := drv.Observe(context.Background(), svc); err == nil {
		t.Fatal("expected an unknown provider to fail")
	}
}
