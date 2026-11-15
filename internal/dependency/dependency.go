// Package dependency provisions the external CLI programs Flynn needs but does not
// ship, such as a hosting provider's CLI. A dependency is a resource whose spec is
// data: how to detect the program, its version floor and pinned per-platform
// artifacts. One engine satisfies any spec and hard-codes no tool.
//
// An installed program at or above the floor is used as-is. Otherwise Flynn
// provisions the pinned build through the acquire path the model runtime uses.
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

// Release is one pinned artifact for one platform: source, digest, archive format
// and executable, as the acquire package needs.
type Release struct {
	// GOOS and GOARCH are the target platform, as runtime.GOOS and runtime.GOARCH.
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
	// BinName is the executable to locate in the extracted archive ("flyctl",
	// "flyctl.exe").
	BinName string `json:"binName"`
}

// Spec describes an external dependency as data with no secret: how to recognize
// it, the refused-below floor, the version to provision and the per-platform
// artifacts.
type Spec struct {
	// Description is a short human summary of what the program is.
	Description string `json:"description,omitempty"`
	// Binaries are the executable names that satisfy the dependency, searched on
	// PATH in preference order (["flyctl", "fly"]).
	Binaries []string `json:"binaries"`
	// VersionArgs make the program print its version (["version"], ["--version"]).
	// Empty skips the probe, so any present binary is accepted without a floor.
	VersionArgs []string `json:"versionArgs,omitempty"`
	// VersionRegex extracts the version from the output from capture group one.
	// Empty parses the whole output.
	VersionRegex string `json:"versionRegex,omitempty"`
	// MinVersion is the floor; a build below it is refused. Empty means no floor.
	MinVersion string `json:"minVersion,omitempty"`
	// Pin is the version provisioned when missing. The build-time gate holds it at
	// or above MinVersion.
	Pin string `json:"pin,omitempty"`
	// Releases are the pinned per-platform artifacts. Provisioning needs one for the
	// current platform.
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

// RegisterKind registers the Dependency kind with reg. It is idempotent.
func RegisterKind(reg *resource.Registry) error { return reg.Register(KindDef) }

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) {
	s, err := resource.DecodeSpec[Spec](r)
	if err != nil {
		return Spec{}, fmt.Errorf("dependency: decode spec: %w", err)
	}
	return s, nil
}

// ReleaseFor returns the release for the platform, or false when s ships none.
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

// Store is the typed dependency facade over a resource.Store. Specs live in the
// instance-global scope, addressed by name.
type Store struct {
	rs    resource.Store
	scope resource.Scope
}

// NewStore returns a facade over rs, whose registry must have the Dependency kind
// (see RegisterKind).
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

func toDependency(r resource.Resource) (Dependency, error) {
	spec, err := DecodeSpec(r)
	if err != nil {
		return Dependency{}, err
	}
	return Dependency{Name: r.Name, Spec: spec}, nil
}
