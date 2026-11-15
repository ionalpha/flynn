package ops

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/resource"
)

// Driver supervises deployed workloads by running their provider's "status" and
// "teardown" hosting operations. Each runs through the same governed transport,
// credential resolution, role check and egress confinement as any operation, so
// supervision is no more privileged than a deploy.
//
// It speaks no provider protocol. What a provider needs to re-address a workload (an
// account id, a project name, a server id) travels on the service's Address map and is
// replayed verbatim into the operation input.
type Driver struct {
	store  resource.Store
	loader *extension.Loader
	scope  resource.Scope

	// mu serializes resolve, load and invoke so the shared loader is never reloaded for
	// another provider mid-call. Supervision is not latency-critical, so this costs
	// little.
	mu sync.Mutex
}

// NewDriver builds a supervision driver over the extensions in store. loader must have
// the ops surface handler registered, as the deploy path's loader does.
func NewDriver(store resource.Store, loader *extension.Loader) *Driver {
	return &Driver{store: store, loader: loader}
}

// Observe runs the provider's "status" operation and reads the workload's health from
// it. A provider with no status operation is not an error: the supervisor keeps the
// last known status and re-checks later.
func (d *Driver) Observe(ctx context.Context, svc service.Service) (service.Observation, error) {
	out, found, err := d.invoke(ctx, svc, OpStatus)
	if err != nil {
		return service.Observation{}, err
	}
	if !found {
		return service.Observation{}, nil
	}
	return parseObservation(out), nil
}

// Teardown runs the provider's "teardown" operation. A provider without one cannot
// retire the workload, a terminal condition the operator resolves (remove it by hand,
// then `flynn services rm`). It is reported once and never retried, so the supervisor
// does not spin.
func (d *Driver) Teardown(ctx context.Context, svc service.Service) error {
	_, found, err := d.invoke(ctx, svc, OpTeardown)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Terminal, "ops_no_teardown",
			"ops: provider "+svc.Spec.Provider+" declares no teardown operation; remove the workload manually then run flynn services rm")
	}
	return nil
}

// invoke runs the provider's named hosting operation with the service's identity
// envelope. found is false when the provider does not declare the operation, which is
// distinct from an error.
func (d *Driver) invoke(ctx context.Context, svc service.Service, opName string) (out string, found bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	r, err := d.resolve(ctx, svc.Spec.Provider)
	if err != nil {
		return "", false, err
	}
	if _, err := d.loader.Load(ctx, r); err != nil {
		return "", false, err
	}
	for _, t := range d.loader.Tools() {
		if t.Def().Name == opName {
			out, err = t.Invoke(ctx, identityInput(svc))
			return out, true, err
		}
	}
	return "", false, nil
}

// resolve finds a provider's extension: usually the one named for the provider, read
// directly, else one whose declared provider matches.
func (d *Driver) resolve(ctx context.Context, provider string) (resource.Resource, error) {
	if r, err := d.store.Get(ctx, extension.Kind, d.scope, provider); err == nil {
		return r, nil
	} else if !errors.Is(err, resource.ErrNotFound) {
		return resource.Resource{}, err
	}
	rs, err := d.store.List(ctx, extension.Kind, d.scope, nil)
	if err != nil {
		return resource.Resource{}, err
	}
	for _, r := range rs {
		spec, decErr := extension.DecodeSpec(r)
		if decErr != nil {
			continue
		}
		if spec.Provider == provider {
			return r, nil
		}
	}
	return resource.Resource{}, fault.New(fault.Terminal, "ops_no_provider",
		"ops: no extension provides "+provider+" (configure it with flynn integrations)")
}

// identityInput builds the input that re-addresses a workload: the external id, live
// URL and service name, plus the provider's Address keys at the top level so an
// operation reads them as config ({{config.account_id}}). An Address key never
// overwrites a reserved identity field.
func identityInput(svc service.Service) json.RawMessage {
	in := map[string]any{
		"name":       svc.Name,
		"externalID": svc.Spec.ExternalID,
		"url":        svc.Spec.URL,
	}
	for k, v := range svc.Spec.Address {
		if _, reserved := in[k]; reserved {
			continue
		}
		in[k] = v
	}
	b, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// parseObservation reads a workload's health from a status result. Result formats vary,
// so it scans common field names and accepts a result wrapped under a "result" key.
// What it cannot read stays empty, which the supervisor treats as unchanged.
func parseObservation(raw string) service.Observation {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return service.Observation{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return service.Observation{}
	}
	if inner, ok := m["result"].(map[string]any); ok {
		m = inner
	}
	return service.Observation{
		Phase: firstString(m, "phase", "status", "state"),
		URL:   firstString(m, "url", "deployment_url", "live_url", "subdomain"),
	}
}

// firstString returns the first key in m whose value is a non-empty string.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Driver implements the supervisor's [service.Driver].
var _ service.Driver = (*Driver)(nil)
