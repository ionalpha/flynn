// Package integrations turns an Extension's "integration" surface into callable
// tools. An integration is a set of named operations sharing one base URL, one auth
// scheme and one governed HTTP transport. Each operation is a declarative flow,
// usually one request, possibly chained steps (fetch, reshape, branch, loop). The
// credential is resolved by name from the vault at call time and never lives in the
// spec.
//
// It composes the Extension model and handler registry, the flow interpreter and the
// shared request transport with its auth providers, so an API integration is an
// Extension with an integration block.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/integrations/auth"
	"github.com/ionalpha/flynn/internal/integrations/request"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/secret"
)

// Spec is the "integration" block of an Extension manifest: the operations it
// exposes. Base URL and auth are on the enclosing Extension spec, declared once for
// every operation.
type Spec struct {
	Operations []Operation `json:"operations"`
}

// Operation is one callable tool. Name and Description are what the model sees;
// Input is the JSON Schema of the arguments the flow reads as config; Flow runs on
// invocation. Role is the required credential role: with a credential store, a
// credential below it is refused before the flow runs. An empty Role requires none.
type Operation struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Role        credential.Role `json:"role,omitempty"`
	Flow        json.RawMessage `json:"flow"`
}

// Denial records a credential refused because its role was below the operation's.
// The audit recorder (WithAudit) receives it, so a host can log it with the
// caller's principal.
type Denial struct {
	Principal      string
	Integration    string
	Credential     string
	CredentialRole credential.Role
	RequiredRole   credential.Role
}

// AuditFunc receives a Denial when a role check refuses a credential. The host wires
// it to its audit log, so this package depends on no log backend.
type AuditFunc func(Denial)

// Handler is the extension.Point for the "integration" surface. It builds
// operations into tools on the flow interpreter and the shared transport, tracks
// them per extension and serves them to the tool bridge. It is safe for concurrent
// use.
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

// WithTransport sets the HTTP transport. The default is a fresh one (anti-SSRF
// dialer, bounded retries, per-host rate limiting).
func WithTransport(t *request.Transport) Option {
	return func(h *Handler) {
		if t != nil {
			h.transport = t
		}
	}
}

// WithSecrets sets the vault auth providers resolve from. The default reads the
// process environment.
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

// WithCredentials sets the credential store that resolves an integration's
// credential by name or default and enforces operation roles. Without it the auth
// credential reference is a direct vault reference and no role is enforced.
func WithCredentials(c *credential.Store) Option {
	return func(h *Handler) { h.creds = c }
}

// WithAudit sets the recorder notified when a role check refuses a credential.
func WithAudit(a AuditFunc) Option {
	return func(h *Handler) { h.audit = a }
}

// WithClock sets the clock oauth2 token expiry is measured on (default
// clock.System).
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

// RegisterWith registers an integration handler on reg, so loading an Extension with
// an integration block yields tools.
func RegisterWith(reg *extension.Registry, opts ...Option) error {
	return reg.Register(NewHandler(opts...))
}

// Capability is the surface key this handler serves.
func (h *Handler) Capability() string { return extension.SurfaceIntegration }

// OnLoad builds the integration's operations into tools. It validates the block,
// every flow and the auth spec up front, so a misconfigured integration fails at load
// instead of at first call.
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

	// The auth configuration is validated here (an api_key needs a parameter name, an
	// oauth2 scheme a token endpoint); the credential value is resolved per call.
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

// Tools returns the tools mounted for an extension id, as extension.ToolSource.
func (h *Handler) Tools(id string) []mission.Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.mounted[id]
	out := make([]mission.Tool, len(ts))
	copy(out, ts)
	return out
}

// opTool is one operation as a mission.Tool. It runs the flow with the tool input as
// config and returns the result as JSON. The credential and request path resolve per
// call.
type opTool struct {
	name    string
	desc    string
	input   json.RawMessage
	flow    flow.Flow
	role    credential.Role
	binding *binding
}

var _ mission.Tool = (*opTool)(nil)

// Def describes the tool to the model. With no input schema it accepts any object.
func (t *opTool) Def() llm.Tool {
	schema := t.input
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return llm.Tool{Name: t.name, Description: t.desc, InputSchema: schema}
}

// Invoke runs the operation flow with the decoded input as config and returns the
// result as JSON. A flow error is returned classified.
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

// binding is the per-extension request context an integration's operations share:
// transport, vault, base URL, egress envelope, auth spec, and optional credential
// store and audit recorder. It resolves the credential and builds the interpreter
// per call, so a default change or credential rotation applies without a reload.
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

	// providerMu guards providers, built auth providers keyed by (auth type, vault
	// reference). The binding outlives every call, so a stateful provider (oauth2)
	// keeps its access token across operations.
	providerMu sync.Mutex
	providers  map[string]auth.Provider
}

// cachedProvider returns the provider for (authType, ref), building it once. Reuse
// keeps oauth2's token cache, which fetches lazily and refreshes near expiry, so a
// fresh provider per call would add a token round trip each time. The static schemes
// resolve their secret on each Apply and hold no state. A rotation changing the
// reference or type keys a new entry; a same-reference rotation is read on the next
// resolve (for oauth2, when the token expires). build makes no network call, so it
// runs under the lock.
func (b *binding) cachedProvider(authType, ref string, build func() (auth.Provider, error)) (auth.Provider, error) {
	key := authType + "\x00" + ref
	b.providerMu.Lock()
	defer b.providerMu.Unlock()
	if p, ok := b.providers[key]; ok {
		return p, nil
	}
	p, err := build()
	if err != nil {
		return nil, err
	}
	if b.providers == nil {
		b.providers = make(map[string]auth.Provider, 1)
	}
	b.providers[key] = p
	return p, nil
}

// run resolves the auth provider, enforcing the required role, and runs the flow.
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
// no credential reference (a public API on "none"), the auth spec is used and no
// role is checked. Otherwise the reference selects a credential by name or default,
// its role must permit the required one (a refusal is audited and returned as
// Forbidden), and the provider uses the credential's vault reference.
func (b *binding) resolveProvider(ctx context.Context, required credential.Role) (auth.Provider, error) {
	if b.creds == nil || b.auth.CredentialRef == "" {
		return b.cachedProvider(b.auth.Type, b.auth.CredentialRef, func() (auth.Provider, error) {
			return providerFor(b.auth, b.transport, b.clk)
		})
	}
	// The credential is the authorization gate, so it is resolved and its role
	// checked every call: a rotation, a role change or a disallowed principal takes
	// effect at once. Only the built provider is cached.
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
	// The credential may override the auth type (providerForCredential), so the key
	// holds the effective type and vault reference and a change rebuilds the
	// provider.
	effType := b.auth.Type
	if cred.Spec.AuthType != "" {
		effType = cred.Spec.AuthType
	}
	return b.cachedProvider(effType, cred.Ref(), func() (auth.Provider, error) {
		return providerForCredential(b.auth, cred, b.transport, b.clk)
	})
}
