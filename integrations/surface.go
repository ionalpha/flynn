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

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/flow"
	"github.com/ionalpha/flynn/integrations/auth"
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
// Role is the credential role the operation requires: when a credential store is
// configured, a credential below this role is refused before the flow runs. An empty
// role requires no particular privilege.
type Operation struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Role        credential.Role `json:"role,omitempty"`
	Flow        json.RawMessage `json:"flow"`
}

// Denial records a credential refused for an operation because its role was below
// the operation's required role. It is handed to the audit recorder (see WithAudit)
// so a host can write it to the event log with the caller's principal.
type Denial struct {
	Principal      string
	Integration    string
	Credential     string
	CredentialRole credential.Role
	RequiredRole   credential.Role
}

// AuditFunc receives a Denial when a role check refuses a credential. The host wires
// it to its audit log; the integrations package stays decoupled from any log backend.
type AuditFunc func(Denial)

// Handler is the extension.Point for the "integration" surface. It builds an
// integration's operations into tools backed by the flow interpreter and the shared
// transport, tracks them per extension, and exposes them through the tool-bridge. It
// is safe for concurrent use.
type Handler struct {
	transport *request.Transport
	secrets   secret.Source
	limits    flow.Limits
	clk       clock.Clock       // measures oauth2 token expiry
	creds     *credential.Store // optional; when set, credentials resolve by name/default and roles are enforced
	audit     AuditFunc         // optional; receives a Denial on a role refusal

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

// WithCredentials sets the credential store used to resolve an integration's
// credential by name or default and to enforce operation roles. Without it, the
// extension's auth credential reference is treated as a direct vault reference and no
// role is enforced (the zero-config behaviour).
func WithCredentials(c *credential.Store) Option {
	return func(h *Handler) { h.creds = c }
}

// WithAudit sets the recorder notified when a role check refuses a credential.
func WithAudit(a AuditFunc) Option {
	return func(h *Handler) { h.audit = a }
}

// WithClock sets the clock oauth2 token expiry is measured against (default
// clock.System). Tests pass a manual clock for determinism.
func WithClock(c clock.Clock) Option {
	return func(h *Handler) {
		if c != nil {
			h.clk = c
		}
	}
}

// NewHandler builds an integration-surface handler.
func NewHandler(opts ...Option) *Handler {
	h := &Handler{
		transport: request.New(),
		secrets:   secret.EnvSource{},
		limits:    flow.DefaultLimits,
		clk:       clock.System{},
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

	// Validate the auth configuration up front (scheme plus its required fields, e.g.
	// an api_key needs a parameter name, an oauth2 scheme needs a token endpoint) so a
	// misconfigured integration is rejected at load. The credential value itself is
	// resolved per call.
	if _, err := providerFor(m.Spec.Auth, h.transport, h.clk); err != nil {
		return err
	}
	b := &binding{
		transport: h.transport,
		secrets:   h.secrets,
		limits:    h.limits,
		clk:       h.clk,
		base:      m.Spec.BaseURL,
		egress:    m.Spec.Safety.EgressAllow,
		auth:      m.Spec.Auth,
		creds:     h.creds,
		audit:     h.audit,
	}

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
		if op.Role != "" && !op.Role.Valid() {
			return fault.New(fault.Terminal, "integration_op_role", "integration: operation "+op.Name+" has an unknown role "+string(op.Role))
		}
		f, err := flow.Decode(op.Flow)
		if err != nil {
			return fault.Wrap(fault.Terminal, "integration_op_flow", fmt.Errorf("operation %q: %w", op.Name, err))
		}
		tools = append(tools, &opTool{
			name:    op.Name,
			desc:    op.Description,
			input:   op.Input,
			flow:    f,
			role:    op.Role,
			binding: b,
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
// JSON. The binding resolves the credential and builds the request path per call.
type opTool struct {
	name    string
	desc    string
	input   json.RawMessage
	flow    flow.Flow
	role    credential.Role
	binding *binding
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
	result, err := t.binding.run(ctx, t.flow, t.role, config)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "integration_result", err)
	}
	return string(out), nil
}

// binding holds the per-extension request context shared by an integration's
// operations: the transport, vault, base URL and egress envelope, the auth spec, and
// (optionally) the credential store and audit recorder. It resolves the credential
// and assembles the flow interpreter on each call, so a default change or a credential
// rotation takes effect without reloading the extension.
type binding struct {
	transport *request.Transport
	secrets   secret.Source
	limits    flow.Limits
	clk       clock.Clock
	base      string
	egress    []string
	auth      extension.AuthSpec
	creds     *credential.Store
	audit     AuditFunc
}

// run resolves the auth provider (enforcing the required role) and runs the flow.
func (b *binding) run(ctx context.Context, f flow.Flow, required credential.Role, config map[string]any) (any, error) {
	provider, err := b.resolveProvider(ctx, required)
	if err != nil {
		return nil, err
	}
	doer := newTransportDoer(b.transport, provider, b.secrets, b.base, b.egress)
	interp := flow.New(flow.WithHTTP(doer), flow.WithLimits(b.limits))
	return interp.Run(ctx, f, config)
}

// resolveProvider builds the auth provider for a call. With no credential store, or
// for an integration that references no credential (a public API authenticating with
// "none"), the auth spec is used directly and no role is checked. Otherwise the
// reference selects a credential by name or default, the credential's role must
// permit the operation's required role (a refusal is audited and returned as a
// Forbidden fault), and the provider is built against the resolved credential's vault
// reference.
func (b *binding) resolveProvider(ctx context.Context, required credential.Role) (auth.Provider, error) {
	if b.creds == nil || b.auth.CredentialRef == "" {
		return providerFor(b.auth, b.transport, b.clk)
	}
	cred, err := b.creds.Resolve(ctx, b.auth.CredentialRef)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "integration_credential", err)
	}
	if !cred.Spec.Role.Permits(required) {
		principal := capability.PrincipalFromContext(ctx)
		if b.audit != nil {
			b.audit(Denial{
				Principal:      principal,
				Integration:    cred.Spec.Integration,
				Credential:     cred.Spec.Name,
				CredentialRole: cred.Spec.Role,
				RequiredRole:   required,
			})
		}
		return nil, fault.New(fault.Forbidden, "credential_role_denied",
			fmt.Sprintf("credential %q with role %q may not perform an action requiring role %q (principal %q)",
				cred.Ref(), cred.Spec.Role, required, principal))
	}
	return providerForCredential(b.auth, cred, b.transport, b.clk)
}
