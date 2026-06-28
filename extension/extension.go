// Package extension defines the one model every runtime-authored capability
// conforms to. An Extension is a typed resource on the event-sourced foundation
// (admitted against a JSON schema, versioned, provenance-stamped, fleet-syncable
// like every other kind) that describes how to reach and use an external system:
// its base URL and protocol, its auth scheme, the capability tags that gate it, a
// safety envelope, and one or more typed surface blocks (an API integration, a
// tool, a hosting/ops provider, a scraper, an auth provider, an agent).
//
// The design goal is a single engine. Integrations, plugins, hosting providers,
// and the agent's own self-authored tools are all the same kind of thing: a spec
// the store holds and a handler interprets. The spec is the default and
// carries no executable code; compiled Go enters only behind the optional-code
// port, referenced from the manifest by name (see CodeRef). New surface types are
// added by registering a handler (see Registry), never by editing this kind or the
// loader, so the agent can extend itself at runtime without a recompile.
package extension

import (
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Extension kind's API group and version. The `.ionagent.io`
	// suffix marks it unmistakably as ours, never a Kubernetes built-in.
	GroupVersion = "extension.ionagent.io/v1alpha1"
	// Kind is the resource kind name extensions are stored under.
	Kind = "Extension"
)

// Well-known surface keys. A surface is one capability an extension exposes; the
// key routes its typed block to the handler registered for it (see Registry).
// These constants name the surfaces the core ships handlers for, but the set is
// open: a host or the agent may register a handler under any key and a spec may
// declare it, with no change to this kind. Decoding a surface's block is the
// handler's job, so the kind schema never constrains an individual block.
const (
	// SurfaceIntegration is an HTTP/JSON API surface driven by an endpoint contract.
	SurfaceIntegration = "integration"
	// SurfaceTool exposes one or more callable tools directly to the agent.
	SurfaceTool = "tool"
	// SurfaceOps is a hosting/operations provider (deploy, provision, supervise).
	SurfaceOps = "ops"
	// SurfaceScrape acquires a spec or data by scraping a documentation site.
	SurfaceScrape = "scrape"
	// SurfaceAuth contributes a named authentication provider other surfaces use.
	SurfaceAuth = "auth"
	// SurfaceAgent declares an agent archetype packaged with the extension.
	SurfaceAgent = "agent"
)

// Spec is an Extension's desired shape: everything needed to reach and use an
// external system, as pure data. Every field is optional so a minimal Extension is
// just a name and one surface; the zero Spec is an inert extension that exposes
// nothing. The extension's natural name (its slug) is the resource Name, so the
// spec carries no separate id.
type Spec struct {
	// DisplayName is a human label; empty falls back to the resource name.
	DisplayName string `json:"displayName,omitempty"`
	// Version is the extension's own published version, used for upgrade ordering and
	// provenance. It is distinct from the resource revision the store assigns: one
	// tracks what the author shipped, the other tracks how many times this record was
	// written.
	Version string `json:"version,omitempty"`
	// Provider groups extensions that share an upstream (e.g. "cloudflare"), so
	// several surfaces and credential roles can be reasoned about as one provider.
	Provider string `json:"provider,omitempty"`

	// BaseURL is the root the surfaces address. Surface blocks carry paths relative to
	// it so the host is declared once.
	BaseURL string `json:"baseURL,omitempty"`
	// Protocol names how the surfaces talk to the upstream. Empty means "https". A
	// protocol the declarative interpreter does not understand (a binary or streaming
	// protocol) is served through the optional-code port; see Code.
	Protocol string `json:"protocol,omitempty"`

	// Auth describes how a request authenticates. It never carries a secret value:
	// the credential is referenced by name and resolved from the vault at call time,
	// so a stored spec is safe to sync and inspect.
	Auth AuthSpec `json:"auth,omitempty"`

	// Capabilities are the tags that gate this extension. A surface is mounted and
	// surfaced as a tool only when its capability is granted to the run and the
	// referenced credential is configured, the same gating every other context is
	// retrieved under.
	Capabilities []string `json:"capabilities,omitempty"`

	// Safety bounds what the extension may do at runtime (egress allow-list, read-only
	// marking, rate limit). The egress waist and capability gate enforce it; the spec
	// only declares the intent.
	Safety SafetySpec `json:"safety,omitempty"`

	// Surfaces are the capability blocks this extension exposes, keyed by surface
	// (see the Surface* constants). Each value is the typed block for that surface,
	// left as raw JSON here because only the surface's handler knows its shape. This
	// is the open extension point: registering a handler under a new key makes specs
	// that declare it loadable, with no change to this kind.
	Surfaces map[string]json.RawMessage `json:"surfaces,omitempty"`

	// Code, when set, routes this extension's surfaces through a compiled
	// implementation registered in-process under the named code port, instead of the
	// declarative interpreter. The manifest carries only the name, never Go, so the
	// spec stays pure data the store can admit, version, and sync. This is the
	// single, explicit boundary at which compiled code enters; pure-spec is the
	// default.
	Code *CodeRef `json:"code,omitempty"`
}

// AuthSpec declares how a surface authenticates without ever embedding a secret.
// The credential value lives in the vault and is resolved by reference at call
// time, so the stored spec is safe to inspect, diff, and sync across a fleet.
type AuthSpec struct {
	// Type is the auth scheme: "none", "basic", "bearer", "api_key", "oauth2", or the
	// name of a code-port auth provider for anything custom (SigV4, a service-account
	// JWT). Empty means "none".
	Type string `json:"type,omitempty"`
	// In and Name place an api_key credential: In is "header" or "query", Name is the
	// header or parameter name. Ignored for other types.
	In   string `json:"in,omitempty"`
	Name string `json:"name,omitempty"`
	// Scheme is the bearer prefix (default "Bearer"); ignored for other types.
	Scheme string `json:"scheme,omitempty"`
	// CredentialRef names the stored credential to resolve from the vault. The
	// multi-key, role-aware credential model is fleshed out by the auth overhaul; here
	// it is the reference the spec carries instead of a value.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Roles enumerates the named credential roles this extension distinguishes (e.g. a
	// read role and a deploy role backed by different keys). Empty means a single
	// default credential.
	Roles []string `json:"roles,omitempty"`
	// OAuth2 carries the static parameters of an "oauth2" scheme (the token endpoint,
	// client id, grant, and scopes). The secret it exchanges (a client secret or a
	// refresh token) is the resolved credential, never carried here. Ignored for other
	// types.
	OAuth2 *OAuth2Spec `json:"oauth2,omitempty"`
}

// OAuth2Spec is the static configuration of an "oauth2" auth scheme. The access
// token is obtained from TokenURL and refreshed automatically; the credential the
// integration holds supplies the client secret (the client_credentials grant) or the
// refresh token (the refresh_token grant), resolved from the vault at call time.
type OAuth2Spec struct {
	// TokenURL is the token endpoint the access token is obtained from.
	TokenURL string `json:"tokenURL,omitempty"`
	// ClientID is the OAuth2 client identifier (semi-public, carried inline).
	ClientID string `json:"clientID,omitempty"`
	// Grant selects the flow: "client_credentials" (default) or "refresh_token".
	Grant string `json:"grant,omitempty"`
	// Scopes are requested at the token endpoint.
	Scopes []string `json:"scopes,omitempty"`
}

// SafetySpec is the declared safety envelope for an extension. It is intent the
// runtime enforces: the egress waist confines network reach to EgressAllow (plus
// the BaseURL host), the capability gate refuses ungranted surfaces, and the
// transport honours the rate limit. A spec cannot weaken these by omission; an
// empty envelope is the most restrictive reading (base-URL host only).
type SafetySpec struct {
	// EgressAllow lists additional hostnames the extension may reach beyond the
	// BaseURL host. Empty confines it to the BaseURL host alone.
	EgressAllow []string `json:"egressAllow,omitempty"`
	// ReadOnly marks an extension whose surfaces never mutate upstream state, so a
	// run granted only read authority may still use it.
	ReadOnly bool `json:"readOnly,omitempty"`
	// RateLimitPerMinute caps requests per minute across the extension's surfaces. 0
	// means the shared transport's default.
	RateLimitPerMinute int `json:"rateLimitPerMinute,omitempty"`
}

// CodeRef names a compiled implementation registered in-process under the
// optional-code port. It is the only way executable behaviour binds to an
// extension, and it binds by name: the manifest stays declarative data while the
// code lives in the binary (or a host plugin), resolved at load time. Resolving an
// unknown name fails closed, so a spec can never silently run the wrong code.
type CodeRef struct {
	// Name is the registered code-port implementation to bind. Required when Code is
	// set.
	Name string `json:"name"`
}

// Status is an Extension's observed state, set by the loader and reconcilers rather
// than admitted against the spec schema. It records how far the extension has been
// validated, whether it is enabled, and which surfaces are currently mounted.
type Status struct {
	// ObservedGeneration is the spec generation the status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Grade is how far the spec has been validated: GradeUnvalidated, GradeSchemaValid
	// (admitted by the kind schema), or GradeProbed (a live probe call succeeded in the
	// sandbox). Acquisition and runtime authoring promote the grade.
	Grade string `json:"grade,omitempty"`
	// LastProbe is the RFC3339 timestamp of the last probe, supplied by the caller's
	// clock (never read from the wall clock here so status stays replay-equivalent).
	// Empty means never probed.
	LastProbe string `json:"lastProbe,omitempty"`
	// Enabled reports whether the extension is currently loaded and its surfaces
	// mounted.
	Enabled bool `json:"enabled,omitempty"`
	// MountedSurfaces lists the surface keys the loader has mounted, in sorted order.
	MountedSurfaces []string `json:"mountedSurfaces,omitempty"`
}

// Validation grades for Status.Grade.
const (
	// GradeUnvalidated is a spec that has not yet passed admission.
	GradeUnvalidated = "unvalidated"
	// GradeSchemaValid is a spec admitted by the kind schema but not probed live.
	GradeSchemaValid = "schema-valid"
	// GradeProbed is a spec confirmed against the upstream by a live probe call.
	GradeProbed = "probed"
)

// specSchema is the JSON Schema an Extension's Spec must satisfy at admission. It
// constrains the envelope (typed fields, no stray keys) but deliberately leaves the
// per-surface blocks unconstrained: each surface's handler validates its own block,
// which is what keeps the set of surfaces open without editing this schema.
var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "displayName": {"type": "string"},
    "version": {"type": "string"},
    "provider": {"type": "string"},
    "baseURL": {"type": "string"},
    "protocol": {"type": "string"},
    "auth": {
      "type": "object",
      "properties": {
        "type": {"type": "string"},
        "in": {"type": "string"},
        "name": {"type": "string"},
        "scheme": {"type": "string"},
        "credentialRef": {"type": "string"},
        "roles": {"type": "array", "items": {"type": "string"}},
        "oauth2": {
          "type": "object",
          "properties": {
            "tokenURL": {"type": "string"},
            "clientID": {"type": "string"},
            "grant": {"type": "string"},
            "scopes": {"type": "array", "items": {"type": "string"}}
          },
          "additionalProperties": false
        }
      },
      "additionalProperties": false
    },
    "capabilities": {"type": "array", "items": {"type": "string"}},
    "safety": {
      "type": "object",
      "properties": {
        "egressAllow": {"type": "array", "items": {"type": "string"}},
        "readOnly": {"type": "boolean"},
        "rateLimitPerMinute": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    },
    "surfaces": {"type": "object"},
    "code": {
      "type": "object",
      "properties": {"name": {"type": "string"}},
      "required": ["name"],
      "additionalProperties": false
    }
  },
  "additionalProperties": false
}`)

// KindDef is the Extension kind definition registered with a resource registry so
// the store admits extensions.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "extension",
	Plural:     "extensions",
}

// RegisterKind registers the Extension kind with reg so a resource store admits
// extensions. It is idempotent: registering again replaces the definition.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource. An empty Spec decodes to the
// zero Spec, so a bare extension is valid.
func DecodeSpec(r resource.Resource) (Spec, error) {
	var s Spec
	if len(r.Spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return Spec{}, fmt.Errorf("extension: decode spec: %w", err)
	}
	return s, nil
}

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) {
	var s Status
	if len(r.Status) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Status, &s); err != nil {
		return Status{}, fmt.Errorf("extension: decode status: %w", err)
	}
	return s, nil
}

// Encode renders the spec for storage as canonical JSON.
func (s Spec) Encode() (json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("extension: encode spec: %w", err)
	}
	return b, nil
}

// Encode renders the status for storage.
func (s Status) Encode() (json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("extension: encode status: %w", err)
	}
	return b, nil
}

// Surface returns the raw block for a surface key and whether the extension
// declares it.
func (s Spec) Surface(key string) (json.RawMessage, bool) {
	raw, ok := s.Surfaces[key]
	return raw, ok
}
