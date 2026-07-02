// Package dependency manages the external command-line programs Flynn needs to operate
// but does not ship in its own binary (for example a hosting provider's CLI). It is the
// generic, spec-driven analogue of the model-runtime provisioner: a dependency is a typed
// resource whose spec is pure data (how to detect it on the host, the minimum version that
// is acceptable, and the pinned per-platform artifacts to install when it is missing), and
// one engine reads any such spec to satisfy it.
//
// The policy is detect-installed-first: a program already present on the host that meets
// the version floor is used as-is, with no download. Only when none is present, or the
// present one is below the floor, does Flynn provision the pinned build, fetched and
// verified through the same hardened acquire path the model runtime uses. The program is
// data here; the engine never hard-codes a tool.
package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Dependency kind's API group and version.
	GroupVersion = "dependency.ionagent.io/v1alpha1"
	// Kind is the resource kind name dependencies are stored under.
	Kind = "Dependency"
)

// ErrNotFound is returned when a dependency spec does not exist.
var ErrNotFound = errors.New("dependency: not found")

// Release is one pinned artifact for a single platform: where to fetch it, the digest it
// must match, its archive format, and the executable inside it. It is the data the acquire
// layer needs to install a verified build, with the platform it targets.
type Release struct {
	// GOOS and GOARCH are the platform this build targets (Go's runtime.GOOS/GOARCH).
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	// URL is the https source of the release archive.
	URL string `json:"url"`
	// SHA256 is the pinned digest the downloaded archive must match.
	SHA256 string `json:"sha256"`
	// SizeBytes is the archive's known size, used as the download cap.
	SizeBytes int64 `json:"sizeBytes"`
	// Archive is the container format: "zip" or "tar.gz".
	Archive string `json:"archive"`
	// BinName is the executable to locate inside the extracted archive (for example
	// "flyctl" or "flyctl.exe").
	BinName string `json:"binName"`
}

// Spec is the desired shape of an external dependency: pure data, no secret. It says how to
// recognize the program on the host, the floor below which a build is refused, the version
// to install when provisioning, and the pinned artifacts to install per platform.
type Spec struct {
	// Description is a short human summary of what the program is.
	Description string `json:"description,omitempty"`
	// Binaries are the executable names that satisfy this dependency, in preference order,
	// searched on PATH for the detect-installed-first check (for example ["flyctl", "fly"]).
	Binaries []string `json:"binaries"`
	// VersionArgs are the arguments that make the program print its version (for example
	// ["version"] or ["--version"]). Empty skips the version probe: any present binary is
	// accepted and no floor is enforced on a system install.
	VersionArgs []string `json:"versionArgs,omitempty"`
	// VersionRegex extracts the version token from the program's version output; capture
	// group one is parsed as the version. Empty parses the whole output.
	VersionRegex string `json:"versionRegex,omitempty"`
	// MinVersion is the floor: a present or provisioned build below it is refused. Empty
	// imposes no floor.
	MinVersion string `json:"minVersion,omitempty"`
	// Pin is the version Flynn provisions when the dependency is missing. It must be at or
	// above MinVersion; the build-time gate enforces this.
	Pin string `json:"pin,omitempty"`
	// Releases are the pinned per-platform artifacts to fetch when provisioning. At least
	// the current platform must be covered for provisioning to be possible there.
	Releases []Release `json:"releases,omitempty"`
}

var specSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string"},
    "binaries": {"type": "array", "items": {"type": "string"}, "minItems": 1},
    "versionArgs": {"type": "array", "items": {"type": "string"}},
    "versionRegex": {"type": "string"},
    "minVersion": {"type": "string"},
    "pin": {"type": "string"},
    "releases": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "goos": {"type": "string"},
          "goarch": {"type": "string"},
          "url": {"type": "string"},
          "sha256": {"type": "string"},
          "sizeBytes": {"type": "integer"},
          "archive": {"type": "string", "enum": ["zip", "tar.gz"]},
          "binName": {"type": "string"}
        },
        "required": ["goos", "goarch", "url", "sha256", "archive", "binName"],
        "additionalProperties": false
      }
    }
  },
  "required": ["binaries"],
  "additionalProperties": false
}`)

// KindDef is the Dependency kind definition registered with a resource registry.
var KindDef = resource.Kind{
	APIVersion: GroupVersion,
	Name:       Kind,
	Schema:     specSchema,
	Singular:   "dependency",
	Plural:     "dependencies",
}

// RegisterKind registers the Dependency kind so a store admits dependency specs. It is
// idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("dependency: decode spec: %w", err)
	}
	return s, nil
}

// ReleaseFor returns the release in s targeting the given platform, or false when the spec
// ships no build for it.
func (s Spec) ReleaseFor(goos, goarch string) (Release, bool) {
	for _, r := range s.Releases {
		if r.GOOS == goos && r.GOARCH == goarch {
			return r, true
		}
	}
	return Release{}, false
}

// Dependency is the typed view of a dependency resource.
type Dependency struct {
	Name string
	Spec Spec
}

// Store is the typed dependency facade over a resource.Store. Dependency specs live in the
// instance-global scope, addressed by name.
type Store struct {
	rs    resource.Store
	scope resource.Scope
}

// NewStore returns a dependency facade over rs. The caller must have registered the
// Dependency kind with the registry rs admits against (see RegisterKind).
func NewStore(rs resource.Store) *Store { return &Store{rs: rs} }

// Put creates or updates the named dependency spec.
func (s *Store) Put(ctx context.Context, name string, spec Spec) (Dependency, error) {
	if name == "" || len(spec.Binaries) == 0 {
		return Dependency{}, errors.New("dependency: a dependency needs a name and at least one binary")
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return Dependency{}, fmt.Errorf("dependency: encode spec: %w", err)
	}
	r, err := s.rs.Put(ctx, resource.Resource{
		APIVersion: GroupVersion,
		Kind:       Kind,
		Name:       name,
		Scope:      s.scope,
		Spec:       raw,
	})
	if err != nil {
		return Dependency{}, err
	}
	return toDependency(r)
}

// Get returns the named dependency spec, or ErrNotFound.
func (s *Store) Get(ctx context.Context, name string) (Dependency, error) {
	r, err := s.rs.Get(ctx, Kind, s.scope, name)
	if err != nil {
		if errors.Is(err, resource.ErrNotFound) {
			return Dependency{}, ErrNotFound
		}
		return Dependency{}, err
	}
	return toDependency(r)
}

// List returns every dependency spec, ordered by name.
func (s *Store) List(ctx context.Context) ([]Dependency, error) {
	rs, err := s.rs.List(ctx, Kind, s.scope, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Dependency, 0, len(rs))
	for _, r := range rs {
		d, err := toDependency(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// toDependency builds the typed view from a stored resource.
func toDependency(r resource.Resource) (Dependency, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Dependency{}, err
	}
	return Dependency{Name: r.Name, Spec: spec}, nil
}
