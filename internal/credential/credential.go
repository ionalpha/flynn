// Package credential stores credential metadata on the resource store: owning
// integration, auth scheme, role, default flag and vault reference. The secret
// value stays in the vault and is resolved only when a request is signed, so a
// [Credential] is safe to list, sync and log. An integration may hold several
// named credentials.
package credential

import (
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Credential kind's API group and version.
	GroupVersion = "credential.ionagent.io/v1alpha1"
	// Kind is the resource kind name credentials are stored under.
	Kind = "Credential"
	// integrationLabel lets a label selector narrow a list to one integration.
	integrationLabel = "credential.ionagent.io/integration"
)

// Role is the privilege level a credential may exercise, ordered read < operator
// < admin. The signing code enforces it through [Role.Permits].
type Role string

const (
	// RoleRead may perform read-only actions.
	RoleRead Role = "read"
	// RoleOperator may perform routine state changes (deploy, provision).
	RoleOperator Role = "operator"
	// RoleAdmin may perform any action, including destructive or account-level ones.
	RoleAdmin Role = "admin"
)

// rank orders the roles; an unknown role ranks 0 and is rejected by Valid.
func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Valid reports whether r is a known role. The empty role is invalid as a stored
// value but means "unscoped" to Permits.
func (r Role) Valid() bool { return r.rank() > 0 }

// Permits reports whether a credential with role r may serve an action that
// requires the given role. An empty requirement admits any credential, so an
// integration without roles is never blocked. Otherwise a role-less credential is
// refused and r must rank at least as high.
func (r Role) Permits(required Role) bool {
	if required == "" {
		return true
	}
	if r == "" {
		return false
	}
	return r.rank() >= required.rank()
}

// Spec is the stored shape of a credential. It holds a vault reference, never a
// secret value.
type Spec struct {
	// Integration is the id of the owning integration (extension).
	Integration string `json:"integration"`
	// Name is unique within the integration (e.g. "prod-token").
	Name string `json:"name"`
	// AuthType is bearer, api_key, basic or oauth2, matching the integration's
	// auth type.
	AuthType string `json:"authType,omitempty"`
	// Role bounds what the credential may be used for. Empty means unscoped.
	Role Role `json:"role,omitempty"`
	// IsDefault marks the credential a bare integration reference selects. At
	// most one per integration is the default.
	IsDefault bool `json:"isDefault,omitempty"`
	// VaultRef is where the secret value is stored. Empty means [VaultRef] of
	// integration and name.
	VaultRef string `json:"vaultRef,omitempty"`
	// Description is an optional human note.
	Description string `json:"description,omitempty"`
}

// Status is a credential's observed state.
type Status struct {
	// LastUsed is the RFC3339 time of the last signed request, from the caller's
	// clock so the record replays identically. Empty means never used.
	LastUsed string `json:"lastUsed,omitempty"`
}

// Credential is the typed view of a credential resource.
type Credential struct {
	ID   string
	Spec Spec
	// Version is the resource revision.
	Version int
}

// VaultRef returns the vault reference used when a [Spec] sets none.
func VaultRef(integration, name string) string { return integration + "/" + name }

// Ref returns the effective vault reference.
func (c Credential) Ref() string {
	if c.Spec.VaultRef != "" {
		return c.Spec.VaultRef
	}
	return VaultRef(c.Spec.Integration, c.Spec.Name)
}

// resourceName matches [VaultRef] on purpose, so a credential's resource name
// and default vault reference agree.
func resourceName(integration, name string) string { return integration + "/" + name }

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "integration": {"type": "string"},
    "name": {"type": "string"},
    "authType": {"type": "string"},
    "role": {"type": "string", "enum": ["read", "operator", "admin"]},
    "isDefault": {"type": "boolean"},
    "vaultRef": {"type": "string"},
    "description": {"type": "string"}
  },
  "required": ["integration", "name"],
  "additionalProperties": false
}`)

// KindDef is the Credential kind definition registered with a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "credential",
	Plural:     "credentials",
}

// RegisterKind registers [KindDef] so a store admits credentials. It is
// idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("credential: decode spec: %w", err)
	}
	return s, nil
}

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) {
	s, err := resource.DecodeStatus[Status](r)
	if err != nil {
		return Status{}, fmt.Errorf("credential: decode status: %w", err)
	}
	return s, nil
}
