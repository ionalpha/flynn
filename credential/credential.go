// Package credential models a stored credential as metadata on the resource store,
// kept strictly separate from the secret value it points at. A Credential records
// which integration it belongs to, what auth scheme it is, the role it may exercise,
// whether it is the integration's default, and the vault reference the secret value
// lives behind. The value itself never enters a Credential: it stays in the vault and
// is resolved by reference only at the point a request is signed, so a credential is
// safe to list, sync, and put on the event log while the secret is not.
//
// This decouples a credential from a provider name. An integration may hold several
// named credentials (a production token, a staging token), each selectable by name
// or by the integration's default, and each carrying a role that bounds what it may
// be used for.
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
	// integrationLabel carries the owning integration id on the resource, so a list
	// can be narrowed to one integration with a label selector.
	integrationLabel = "credential.ionagent.io/integration"
)

// Role is the privilege level a credential may exercise. Roles are ordered
// read < operator < admin, and they are an opt-in narrowing: a credential with no
// role is unscoped and an action that requires no role admits any credential. A
// credential is refused only when both it and the action name a role and the
// credential's rank is below the action's. The enforcement (refusing a low-role
// credential for a higher action, recorded on the event log) is applied at the
// dispatch boundary; this type is the policy the boundary reads.
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

// Valid reports whether r is one of the known roles. The empty role is not valid as
// a stored value (a stored role must be explicit) but is meaningful as "unscoped"
// when passed to Permits.
func (r Role) Valid() bool { return r.rank() > 0 }

// Permits reports whether a credential carrying this role may be used for an action
// that requires the given role. An unscoped credential (empty role) permits any
// action, and an action that requires no role (empty) admits any credential; when
// both name a role, the credential must rank at least as high as the action.
func (r Role) Permits(required Role) bool {
	if r == "" || required == "" {
		return true
	}
	return r.rank() >= required.rank()
}

// Spec is the stored shape of a credential: pure metadata. It never carries a secret
// value, only the vault reference to one.
type Spec struct {
	// Integration is the id of the integration (extension) this credential belongs to.
	Integration string `json:"integration"`
	// Name is the credential's name within its integration (e.g. "prod-token").
	Name string `json:"name"`
	// AuthType is the auth scheme the credential is for (bearer, api_key, basic,
	// oauth2). It mirrors the integration's auth type so a credential and the request
	// it signs agree on the mechanism.
	AuthType string `json:"authType,omitempty"`
	// Role bounds what the credential may be used for. Empty means unscoped.
	Role Role `json:"role,omitempty"`
	// IsDefault marks the credential selected when an integration is referenced
	// without a credential name. At most one credential per integration is the default.
	IsDefault bool `json:"isDefault,omitempty"`
	// VaultRef is the reference the secret value is stored under in the vault. Empty
	// defaults to "<integration>/<name>" (see VaultRef).
	VaultRef string `json:"vaultRef,omitempty"`
	// Description is an optional human note.
	Description string `json:"description,omitempty"`
}

// Status is a credential's observed state.
type Status struct {
	// LastUsed is the RFC3339 time the credential was last used to sign a request,
	// supplied by the caller's clock (never read from the wall clock here, so the
	// record stays replay-equivalent). Empty means never used.
	LastUsed string `json:"lastUsed,omitempty"`
}

// Credential is the typed view of a credential resource: its identity, spec, and
// the resource bookkeeping a caller needs.
type Credential struct {
	ID   string
	Spec Spec
	// Version is the resource revision.
	Version int
}

// VaultRef returns the conventional vault reference for a credential, the form a
// credential's secret is stored under when its spec sets no explicit VaultRef.
func VaultRef(integration, name string) string { return integration + "/" + name }

// Ref returns the credential's effective vault reference: its explicit VaultRef, or
// the conventional "<integration>/<name>" when unset.
func (c Credential) Ref() string {
	if c.Spec.VaultRef != "" {
		return c.Spec.VaultRef
	}
	return VaultRef(c.Spec.Integration, c.Spec.Name)
}

// resourceName is the resource Name a credential is addressed by: "<integration>/<name>",
// unique within the kind and matching the conventional vault reference.
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

// RegisterKind registers the Credential kind so a store admits credentials. It is
// idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	var s Spec
	if len(r.Spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return Spec{}, fmt.Errorf("credential: decode spec: %w", err)
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
		return Status{}, fmt.Errorf("credential: decode status: %w", err)
	}
	return s, nil
}
