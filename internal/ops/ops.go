// Package ops turns an Extension's "ops" surface into a hosting provider: the deploy,
// status, logs, list, teardown and optional provision operations a provider such as
// Cloudflare, Vercel or Hetzner exposes. An ops block is an API integration's operation
// model plus a hosting contract (well-known operation names the operator drives the
// same way on every provider) and targets (static site, container, VPS), so a deploy
// can pick a provider that satisfies a goal.
//
// The handler translates its block into an integration block and delegates to the
// integration handler, so operation mechanics, credentials, role enforcement and egress
// confinement have one implementation.
package ops

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/mission"
)

// OpDeploy is the one operation every hosting provider must declare. By convention it
// returns the provider's id and the live URL.
const OpDeploy = "deploy"

// Optional hosting-contract operations. A provider declares what it supports, and each
// one present is driven by these names so every provider is treated the same.
const (
	OpStatus    = "status"    // report a workload's state
	OpLogs      = "logs"      // fetch a workload's logs
	OpList      = "list"      // list the provider's workloads
	OpTeardown  = "teardown"  // remove a workload
	OpProvision = "provision" // create raw compute (VPS providers)
)

// Spec is the typed "ops" surface block of an Extension manifest. It is an
// integration's operation set plus the provider's hosting targets.
type Spec struct {
	// Targets are what the provider can host, so a deploy can select it for a goal.
	// Empty leaves it unclassified.
	Targets []service.Target `json:"targets,omitempty"`
	// Operations are the provider's hosting operations, each a declarative flow as in an
	// integration. A "deploy" operation is required.
	Operations []integrations.Operation `json:"operations"`
}

// Handler is the [extension.Point] for the "ops" surface. It validates the hosting
// contract and targets, then delegates operations to an internal integration handler,
// so credential, role and egress handling stay in one place. It is safe for concurrent
// use.
type Handler struct {
	inner *integrations.Handler

	mu      sync.Mutex
	targets map[string][]service.Target // extension id -> declared targets
}

// NewHandler builds an ops-surface handler. opts are integration options for the
// internal operation engine, so an ops provider resolves credentials and enforces roles
// as an API integration does.
func NewHandler(opts ...integrations.Option) *Handler {
	return &Handler{
		inner:   integrations.NewHandler(opts...),
		targets: map[string][]service.Target{},
	}
}

// RegisterWith registers an ops-surface handler on an extension registry.
func RegisterWith(reg *extension.Registry, opts ...integrations.Option) error {
	return reg.Register(NewHandler(opts...))
}

// Capability is the surface key this handler serves.
func (h *Handler) Capability() string { return extension.SurfaceOps }

// OnLoad validates the ops block and mounts its operations through the internal
// integration handler. A missing deploy operation, an unknown target or a malformed
// flow is rejected at load, before any deploy.
func (h *Handler) OnLoad(ctx context.Context, m extension.Mount) error {
	var spec Spec
	if len(m.Block) > 0 {
		if err := json.Unmarshal(m.Block, &spec); err != nil {
			return fault.Wrap(fault.Terminal, "ops_decode", err)
		}
	}
	if len(spec.Operations) == 0 {
		return fault.New(fault.Terminal, "ops_no_ops", "ops: surface declares no operations")
	}
	hasDeploy := false
	for _, op := range spec.Operations {
		if op.Name == OpDeploy {
			hasDeploy = true
			break
		}
	}
	if !hasDeploy {
		return fault.New(fault.Terminal, "ops_no_deploy", "ops: a hosting provider must declare a \"deploy\" operation")
	}
	for _, t := range spec.Targets {
		if !t.Valid() {
			return fault.New(fault.Terminal, "ops_bad_target", "ops: unknown target "+string(t))
		}
	}

	// The integration mount keeps the extension's id, name and spec (base URL, auth,
	// safety), so credentials and egress confinement match an API integration.
	block, err := json.Marshal(integrations.Spec{Operations: spec.Operations})
	if err != nil {
		return fault.Wrap(fault.Terminal, "ops_reencode", err)
	}
	inner := m
	inner.Surface = extension.SurfaceIntegration
	inner.Block = block
	if err := h.inner.OnLoad(ctx, inner); err != nil {
		return err
	}

	h.mu.Lock()
	h.targets[m.ID] = append([]service.Target(nil), spec.Targets...)
	h.mu.Unlock()
	return nil
}

// OnUnload drops the operations and targets mounted for an extension. It is idempotent.
func (h *Handler) OnUnload(ctx context.Context, id string) error {
	h.mu.Lock()
	delete(h.targets, id)
	h.mu.Unlock()
	return h.inner.OnUnload(ctx, id)
}

// Tools returns the provider's operations as tools, so the agent can drive a deploy
// in a goal run and the operator surface can call them directly.
func (h *Handler) Tools(id string) []mission.Tool { return h.inner.Tools(id) }

// Targets reports the hosting targets extension id declared.
func (h *Handler) Targets(id string) []service.Target {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.targets[id]
	out := make([]service.Target, len(ts))
	copy(out, ts)
	return out
}

// DeployTool returns the provider's deploy operation as a tool for the operator, or nil
// when the extension is not loaded.
func (h *Handler) DeployTool(id string) mission.Tool {
	for _, t := range h.inner.Tools(id) {
		if t.Def().Name == OpDeploy {
			return t
		}
	}
	return nil
}

// Handler implements [extension.Point] and [extension.ToolSource].
var (
	_ extension.Point      = (*Handler)(nil)
	_ extension.ToolSource = (*Handler)(nil)
)
