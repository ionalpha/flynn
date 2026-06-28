// Package integrations turns an Extension's "integration" surface into callable
// tools. An integration is a set of named operations that share one base URL, one
// auth scheme, and one governed HTTP transport. Each operation is a declarative
// flow: the common case is a single request, but an operation may chain steps
// (fetch, reshape, branch, loop) without any compiled code. Loading an Extension
// that declares an integration surface mounts its operations as tools the agent can
// call; the credential is resolved by name from the vault at call time and never
// lives in the spec.
//
// This package is the bridge between three lower layers: the Extension model and
// its handler registry, the declarative flow interpreter, and the shared request
// transport with its auth providers. It owns none of those mechanisms; it composes
// them so that "an API integration" is just an Extension with an integration block.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/flow"
	"github.com/ionalpha/flynn/integrations/request"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/secret"
)

// Spec is the typed "integration" surface block of an Extension manifest: the set
// of operations the integration exposes. The connection details (base URL, auth)
// live on the enclosing Extension spec and are shared by every operation, so they
// are declared once.
type Spec struct {
	Operations []Operation `json:"operations"`
}

// Operation is one callable tool. Name and Description are what the model sees;
// Input is the JSON Schema for the tool's arguments (the values the flow reads as
// config); Flow is the declarative procedure that runs when the tool is invoked.
type Operation struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Flow        json.RawMessage `json:"flow"`
}

// Handler is the extension.Point for the "integration" surface. It builds an
// integration's operations into tools backed by the flow interpreter and the shared
// transport, tracks them per extension, and exposes them through the tool-bridge. It
// is safe for concurrent use.
type Handler struct {
	transport *request.Transport
	secrets   secret.Source
	limits    flow.Limits

	mu      sync.Mutex
	mounted map[string][]mission.Tool // extension id -> tools
}

// Option configures a Handler.
type Option func(*Handler)

// WithTransport sets the HTTP transport requests are dispatched through. The default
// is a fresh transport (anti-SSRF dialer, bounded retries, per-host rate limiting).
func WithTransport(t *request.Transport) Option {
	return func(h *Handler) {
		if t != nil {
			h.transport = t
		}
	}
}

// WithSecrets sets the vault the auth providers resolve credentials from. The
// default reads the process environment.
func WithSecrets(s secret.Source) Option {
	return func(h *Handler) {
		if s != nil {
			h.secrets = s
		}
	}
}

// WithLimits sets the resource caps every operation flow runs under.
func WithLimits(l flow.Limits) Option {
	return func(h *Handler) { h.limits = l }
}

// NewHandler builds an integration-surface handler.
func NewHandler(opts ...Option) *Handler {
	h := &Handler{
		transport: request.New(),
		secrets:   secret.EnvSource{},
		limits:    flow.DefaultLimits,
		mounted:   map[string][]mission.Tool{},
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// RegisterWith registers an integration-surface handler on an extension registry, so
// a store loading an Extension with an integration block turns it into tools.
func RegisterWith(reg *extension.Registry, opts ...Option) error {
	return reg.Register(NewHandler(opts...))
}

// Capability is the surface key this handler serves.
func (h *Handler) Capability() string { return extension.SurfaceIntegration }

// OnLoad builds the integration's operations into tools. It validates the surface
// block and every operation flow up front, and constructs the auth provider from the
// extension's auth spec, so a misconfigured integration is rejected at load rather
// than failing at first call.
func (h *Handler) OnLoad(_ context.Context, m extension.Mount) error {
	var spec Spec
	if len(m.Block) > 0 {
		if err := json.Unmarshal(m.Block, &spec); err != nil {
			return fault.Wrap(fault.Terminal, "integration_decode", err)
		}
	}
	if len(spec.Operations) == 0 {
		return fault.New(fault.Terminal, "integration_no_ops", "integration: surface declares no operations")
	}

	provider, err := providerFor(m.Spec.Auth)
	if err != nil {
		return err
	}
	doer := newTransportDoer(h.transport, provider, h.secrets, m.Spec.BaseURL, m.Spec.Safety.EgressAllow)
	interp := flow.New(flow.WithHTTP(doer), flow.WithLimits(h.limits))

	tools := make([]mission.Tool, 0, len(spec.Operations))
	seen := map[string]bool{}
	for i := range spec.Operations {
		op := spec.Operations[i]
		if op.Name == "" {
			return fault.New(fault.Terminal, "integration_op_no_name", "integration: an operation has no name")
		}
		if seen[op.Name] {
			return fault.New(fault.Terminal, "integration_op_dup", "integration: duplicate operation "+op.Name)
		}
		seen[op.Name] = true
		f, err := flow.Decode(op.Flow)
		if err != nil {
			return fault.Wrap(fault.Terminal, "integration_op_flow", fmt.Errorf("operation %q: %w", op.Name, err))
		}
		tools = append(tools, &opTool{
			name:   op.Name,
			desc:   op.Description,
			input:  op.Input,
			flow:   f,
			interp: interp,
		})
	}

	h.mu.Lock()
	h.mounted[m.ID] = tools
	h.mu.Unlock()
	return nil
}

// OnUnload drops the tools mounted for an extension. It is idempotent.
func (h *Handler) OnUnload(_ context.Context, id string) error {
	h.mu.Lock()
	delete(h.mounted, id)
	h.mu.Unlock()
	return nil
}

// Tools returns the tools mounted for an extension id, satisfying
// extension.ToolSource so the loader surfaces them to the agent.
func (h *Handler) Tools(id string) []mission.Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.mounted[id]
	out := make([]mission.Tool, len(ts))
	copy(out, ts)
	return out
}

// opTool is one operation exposed as a mission.Tool. Invoking it runs the
// operation's flow with the tool input as config and returns the flow result as
// JSON.
type opTool struct {
	name   string
	desc   string
	input  json.RawMessage
	flow   flow.Flow
	interp *flow.Interpreter
}

var _ mission.Tool = (*opTool)(nil)

// Def describes the tool to the model. An operation with no declared input schema
// accepts a free-form object.
func (t *opTool) Def() llm.Tool {
	schema := t.input
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return llm.Tool{Name: t.name, Description: t.desc, InputSchema: schema}
}

// Invoke runs the operation flow with the decoded input as config and returns the
// result encoded as JSON. A flow error (a failed request, a tripped cap) is returned
// to the caller classified, never swallowed.
func (t *opTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	config := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &config); err != nil {
			return "", fault.Wrap(fault.Terminal, "integration_input", err)
		}
	}
	result, err := t.interp.Run(ctx, t.flow, config)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "integration_result", err)
	}
	return string(out), nil
}
