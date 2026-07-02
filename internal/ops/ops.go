// Package ops turns an Extension's "ops" surface into a hosting/operations provider:
// the deploy, status, logs, list, teardown (and optional provision) operations a
// provider like Cloudflare, Vercel, or Hetzner exposes. An ops surface is the same
// declarative-operation model as an API integration, with two additions that make a
// fleet of providers interchangeable: a small hosting CONTRACT (well-known operation
// names the operator surface can drive uniformly) and TARGETS (what the provider can
// host: a static site, a container, a VPS), so a deploy command can pick a provider
// that satisfies a goal.
//
// The operation mechanics, credential resolution, role enforcement, and egress
// confinement are not reimplemented here: the handler translates its block into an
// integration block and delegates to the integration surface, so there is exactly one
// implementation of "run an authenticated, role-checked, egress-bounded operation
// flow". This package adds the contract and target classification on top.
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

// OpDeploy is the one operation every hosting provider must declare: it stands a
// workload up and (by convention) returns the provider's id and the live URL. The
// operator surface drives it by this name across providers.
const OpDeploy = "deploy"

// Well-known hosting-contract operations beyond deploy. They are not required (a
// provider may expose only what it supports), but when present they are driven by
// these names so the operator surface and the agent treat every provider the same.
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
	// Targets are what this provider can host (static-site, container, vps), so a
	// deploy can select a provider that can satisfy a goal. Empty leaves it
	// unclassified.
	Targets []service.Target `json:"targets,omitempty"`
	// Operations are the provider's hosting operations, each a declarative flow, in the
	// same shape an integration surface uses. At least a "deploy" operation is required.
	Operations []integrations.Operation `json:"operations"`
}

// Handler is the extension.Point for the "ops" surface. It validates the hosting
// contract and the provider's targets, then delegates the operation mechanics to an
// internal integration handler so credential, role, and egress handling live in one
// place. It is safe for concurrent use.
type Handler struct {
	inner *integrations.Handler

	mu      sync.Mutex
	targets map[string][]service.Target // extension id -> declared targets
}

// NewHandler builds an ops-surface handler. The options are the same integration
// options (transport, secrets, credentials, audit, limits, clock) and configure the
// internal operation engine, so an ops provider resolves credentials and enforces
// roles exactly as an API integration does.
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

// OnLoad validates the ops block (the hosting contract and targets), then mounts its
// operations through the internal integration handler. A provider that declares no
// deploy operation, an unknown target, or a malformed operation flow is rejected at
// load rather than at first deploy.
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

	// Re-express the ops operations as an integration block and delegate. The mount
	// keeps the same extension id, name, spec (base URL, auth, safety), so credential
	// resolution and egress confinement are identical to an API integration.
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

// Targets reports the hosting targets an extension declared, so a deploy can pick a
// provider that can satisfy a goal.
func (h *Handler) Targets(id string) []service.Target {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.targets[id]
	out := make([]service.Target, len(ts))
	copy(out, ts)
	return out
}

// DeployTool returns the provider's deploy operation as a tool, or nil if the
// extension is not loaded. It is the entry point the operator surface invokes.
func (h *Handler) DeployTool(id string) mission.Tool {
	for _, t := range h.inner.Tools(id) {
		if t.Def().Name == OpDeploy {
			return t
		}
	}
	return nil
}

// guard: a Handler must satisfy the extension Point and ToolSource interfaces.
var (
	_ extension.Point      = (*Handler)(nil)
	_ extension.ToolSource = (*Handler)(nil)
)
