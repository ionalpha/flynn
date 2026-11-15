// Package extension defines the Extension resource kind: a schema-admitted,
// versioned, fleet-synced spec describing how to reach an external system (base
// URL, protocol, auth, gating capabilities, safety envelope) and the typed
// surface blocks it exposes.
//
// Integrations, plugins, hosting providers and self-authored tools are all an
// Extension spec that a handler interprets. A spec carries no code; compiled Go
// binds only by name through [CodeRef]. A new surface type is a new handler in
// a [Registry], so the agent extends itself without a recompile.
package extension

import (
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the kind's API group and version; the domain suffix keeps
	// it apart from Kubernetes built-ins.
	GroupVersion = "extension.ionagent.io/v1alpha1"
	// Kind is the resource kind name extensions are stored under.
	Kind = "Extension"
)

// Surface keys the core ships handlers for. A key routes a surface block to
// its handler in a [Registry]. The set is open: any key with a registered
// handler works, and the handler decodes its own block.
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

// Spec is an Extension's desired shape, as pure data. Every field is optional;
// the zero Spec exposes nothing. The resource Name is the extension's slug.
type Spec struct {
	// DisplayName is a human label; empty falls back to the resource name.
	DisplayName string `json:"displayName,omitempty"`
	// Version is the author's published version, for upgrade ordering. It is
	// separate from the store's resource revision.
	Version string `json:"version,omitempty"`
	// Provider groups extensions sharing an upstream, e.g. "cloudflare".
	Provider string `json:"provider,omitempty"`

	// BaseURL is the root that surface paths are relative to.
	BaseURL string `json:"baseURL,omitempty"`
	// Protocol is the upstream protocol; empty means "https". One the
	// declarative interpreter cannot speak needs Code.
	Protocol string `json:"protocol,omitempty"`

	// Auth describes how requests authenticate. It names the credential and
	// never holds a secret, so a stored spec is safe to sync.
	Auth AuthSpec `json:"auth,omitempty"`

	// Capabilities gate the extension: a surface is mounted only when its
	// capability is granted to the run and its credential is configured.
	Capabilities []string `json:"capabilities,omitempty"`

	// Safety declares runtime bounds, which the egress waist and capability
	// gate enforce.
	Safety SafetySpec `json:"safety,omitempty"`

	// Surfaces maps a surface key to its block, kept raw because only the
	// key's handler knows the shape.
	Surfaces map[string]json.RawMessage `json:"surfaces,omitempty"`

	// Code, when set, routes the surfaces through a compiled implementation
	// registered in-process under that name. It is the only way compiled code
	// enters an extension.
	Code *CodeRef `json:"code,omitempty"`
}

// AuthSpec declares how a surface authenticates. The secret stays in the vault
// and is resolved by reference at call time.
type AuthSpec struct {
	// Type is "none" (the default), "basic", "bearer", "api_key", "oauth2",
	// or the name of a code-port auth provider such as SigV4.
	Type string `json:"type,omitempty"`
	// In ("header" or "query") and Name place an api_key credential.
	In   string `json:"in,omitempty"`
	Name string `json:"name,omitempty"`
	// Scheme is the bearer prefix, default "Bearer".
	Scheme string `json:"scheme,omitempty"`
	// CredentialRef names the vault credential to resolve.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Roles names distinct credential roles, such as read and deploy; empty
	// means one default credential.
	Roles []string `json:"roles,omitempty"`
	// OAuth2 holds an "oauth2" scheme's static parameters; its secret is the
	// resolved credential.
	OAuth2 *OAuth2Spec `json:"oauth2,omitempty"`
}

// OAuth2Spec configures an "oauth2" scheme. The access token comes from
// TokenURL and is refreshed automatically; the credential supplies the client
// secret or refresh token, by grant.
type OAuth2Spec struct {
	TokenURL string `json:"tokenURL,omitempty"`
	// ClientID is semi-public, so it is carried inline.
	ClientID string `json:"clientID,omitempty"`
	// Grant is "client_credentials" (default) or "refresh_token".
	Grant  string   `json:"grant,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// SafetySpec is the safety envelope the runtime enforces: egress is confined
// to the BaseURL host plus EgressAllow, ungranted surfaces are refused, and the
// rate limit is honoured. Omission never weakens it; empty is the strictest.
type SafetySpec struct {
	// EgressAllow lists hosts reachable beyond the BaseURL host.
	EgressAllow []string `json:"egressAllow,omitempty"`
	// ReadOnly marks surfaces that never mutate upstream, so a read-only run
	// may use them.
	ReadOnly bool `json:"readOnly,omitempty"`
	// RateLimitPerMinute caps requests across all surfaces; 0 is the shared
	// transport's default.
	RateLimitPerMinute int `json:"rateLimitPerMinute,omitempty"`
}

// CodeRef binds an extension by name to compiled code registered in-process,
// resolved at load time. An unknown name fails closed, so a spec cannot run
// the wrong code.
type CodeRef struct {
	// Name is the registered implementation; required.
	Name string `json:"name"`
}

// Status is an Extension's observed state, set by the loader and reconcilers
// and never admitted against the spec schema.
type Status struct {
	// ObservedGeneration is the spec generation the status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Grade is one of the Grade constants, raised by acquisition and runtime
	// authoring.
	Grade string `json:"grade,omitempty"`
	// LastProbe is the RFC3339 time of the last probe from the caller's clock,
	// so status replays identically. Empty means never probed.
	LastProbe string `json:"lastProbe,omitempty"`
	// Enabled reports whether the extension is loaded and mounted.
	Enabled bool `json:"enabled,omitempty"`
	// MountedSurfaces lists mounted surface keys, sorted.
	MountedSurfaces []string `json:"mountedSurfaces,omitempty"`
}

// Validation grades for Status.Grade.
const (
	// GradeUnvalidated has not passed admission.
	GradeUnvalidated = "unvalidated"
	// GradeSchemaValid passed the kind schema and has not been probed.
	GradeSchemaValid = "schema-valid"
	// GradeProbed passed a live probe against the upstream in the sandbox.
	GradeProbed = "probed"
)

// specSchema admits a Spec with typed fields and no stray keys. Surface blocks
// are left open because each handler validates its own, which keeps the set
// of surfaces open.
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

// KindDef is the Extension kind definition for a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "extension",
	Plural:     "extensions",
}

// RegisterKind registers [KindDef] with reg, replacing any earlier one.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec; an empty one gives the zero Spec.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("extension: decode spec: %w", err)
	}
	return s, nil
}

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) {
	s, err := resource.DecodeStatus[Status](r)
	if err != nil {
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

// Surface returns the raw block for key and whether it is declared.
func (s Spec) Surface(key string) (json.RawMessage, bool) {
	raw, ok := s.Surfaces[key]
	return raw, ok
}
