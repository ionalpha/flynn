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

// Driver supervises deployed workloads by running their provider's hosting-contract
// operations. It is the bridge from the provider-agnostic supervisor to a concrete
// provider: given a service, it resolves the extension that provider belongs to, loads
// it, and invokes the well-known "status" or "teardown" operation. The operation runs
// through the same governed transport, credential resolution, role check, and egress
// confinement every operation uses, so supervision is no more privileged than a deploy.
//
// It speaks no provider protocol of its own. What a provider needs to re-address its
// workload (an account id, a project name, a server id) travels on the service's
// Address map, which the driver replays into the operation input verbatim.
type Driver struct {
	store  resource.Store
	loader *extension.Loader
	scope  resource.Scope

	// mu serializes resolve+load+invoke so a single shared loader is never reloaded for
	// a second provider mid-call. Supervision is not latency-critical, so serializing
	// provider calls is an acceptable cost for not racing the loader's mount table.
	mu sync.Mutex
}

// NewDriver builds an ops-backed supervision driver. store is the resource store the
// extensions live in; loader resolves an extension's surfaces to callable tools (it
// must have the ops surface handler registered, the same loader the deploy path uses).
func NewDriver(store resource.Store, loader *extension.Loader) *Driver {
	return &Driver{store: store, loader: loader}
}

// Observe runs the provider's "status" operation and reads the workload's health from
// the result. A provider that declares no status operation cannot be observed, which is
// not an error: the supervisor keeps the last known status and re-checks later.
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

// Teardown runs the provider's "teardown" operation to remove the workload. A provider
// that declares no teardown operation cannot retire the workload itself: that is a
// terminal condition the operator must resolve (remove the workload by hand, then
// `flynn services rm`), reported rather than retried so the supervisor does not spin.
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

// invoke resolves the provider's extension, loads it, and runs its named hosting
// operation with the service's identity envelope. It reports found=false when the
// provider does not declare that operation, distinct from an invocation error.
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

// resolve finds the extension a provider belongs to. The common case is an extension
// whose name is the provider, so it tries a direct read first; failing that it scans
// for one whose declared provider matches, which covers a provider exposed under a
// different extension name.
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

// identityInput builds the operation input that re-addresses a workload: the recorded
// external id, live URL, and service name, plus the provider's own opaque Address keys
// flattened to the top level so a provider operation reads them as ordinary config
// (e.g. {{config.account_id}}). Address keys never overwrite the reserved identity
// fields.
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

// parseObservation reads a workload's health from a status operation's result. Provider
// result shapes vary, so it scans the common field names and tolerates a result wrapped
// under a "result" key. Anything it cannot read is left empty, which the supervisor
// treats as "unchanged".
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

// guard: a Driver satisfies the supervisor's provider boundary.
var _ service.Driver = (*Driver)(nil)
