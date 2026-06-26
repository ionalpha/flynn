// Package catalog is the model catalog: a vetted, versioned list of models with
// provenance, so a user (or the agent) can discover, compare, and choose a model on
// evidence instead of guessing. The adapter registry (provider) knows how to talk to
// a provider:model; this package knows which models exist, what they cost in size and
// context, what they can do, and where they came from.
//
// The catalog ships as data, not code: a models.json embedded in the binary so it
// works offline, authored and reviewed in the public repository, and intended to be
// published as a public artifact that can update without a rebuild. Every entry
// carries a provenance record and a license, and is held at a trust tier, so the list
// is auditable and nothing is offered as if vetted when it is not.
//
// This package is the discovery foundation. Hardware-fit ranking, fetch-to-run, the
// per-model eval, and live enrichment from upstream sources build on the same schema.
package catalog

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Kind is how a model is reached: a hosted API, or local weights run on this machine.
type Kind string

const (
	// KindAPI is a model served behind a provider API (no weights to download).
	KindAPI Kind = "api"
	// KindLocal is open weights run by a local model runtime or inference server.
	KindLocal Kind = "local"
)

// Trust is how far an entry has been vetted. It is surfaced, never hidden, so a model
// is never presented as trusted when it is merely known.
type Trust string

const (
	// TrustBlessed is vetted and shipped in the curated catalog, with full provenance.
	TrustBlessed Trust = "blessed"
	// TrustDiscovered came from an upstream source this run: metadata only, not vetted.
	TrustDiscovered Trust = "discovered"
	// TrustLocal is already present on this machine (detected on disk).
	TrustLocal Trust = "local"
)

// Format is a weight file's encoding, which determines whether loading it is safe.
type Format string

const (
	// FormatSafetensors stores tensors only and executes no code on load (preferred).
	FormatSafetensors Format = "safetensors"
	// FormatGGUF is the quantized single-file format local runtimes use; tensors plus
	// metadata, no code execution.
	FormatGGUF Format = "gguf"
	// FormatPickle (.bin/.pt) can execute arbitrary code when loaded, so it is unsafe
	// and an entry using it is flagged, never silently fetched.
	FormatPickle Format = "pickle"
)

// safe reports whether a weight format executes no code on load.
func (f Format) safe() bool { return f == FormatSafetensors || f == FormatGGUF }

// Source is where a model came from, so an entry can be traced to an authoritative
// publisher rather than hearsay.
type Source struct {
	// Publisher is the first-party namespace that released the model (e.g. "Qwen",
	// "meta-llama", "Anthropic"). Preferring publisher orgs over reuploads is part of
	// the trust check.
	Publisher string `json:"publisher"`
	// URL is the official model card or release page the entry was taken from.
	URL string `json:"url"`
	// Registry is the catalog or service the weights or the API come from, recorded so
	// the entry can be traced back to its origin.
	Registry string `json:"registry"`
}

// Quant is one downloadable quantization of a local model: its on-disk cost, the
// reference a runtime pulls it by, and the content digest used to verify the download.
type Quant struct {
	// Name is the quantization label (e.g. "Q4_K_M", "fp16").
	Name string `json:"name"`
	// Format is the weight encoding; an unsafe format is refused at fetch.
	Format Format `json:"format"`
	// SizeBytes is the on-disk size, the headline number for hardware fit.
	SizeBytes int64 `json:"sizeBytes"`
	// Ref is the pull reference a runtime fetches this quantization by (a registry tag
	// or a file path).
	Ref string `json:"ref"`
	// Digest is the content hash (e.g. "sha256:...") the download is verified against.
	// Empty means unpinned: the fetcher pins it from the registry's own manifest at
	// pull time and still verifies the bytes, so an unpinned entry is never trusted
	// blindly.
	Digest string `json:"digest,omitempty"`
}

// Capabilities is what a model can do, used to filter to models that fit a task.
type Capabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// ModelSpec is one catalog entry: how to select it, where it came from, what it costs,
// and what it can do.
type ModelSpec struct {
	// ID is the provider:model selector the adapter registry resolves, e.g.
	// "anthropic:claude-opus-4-8" for an API model or a "runtime:model" form for a
	// local one.
	ID string `json:"id"`
	// Name is the human-readable model name.
	Name string `json:"name"`
	// Kind is API or local.
	Kind Kind `json:"kind"`
	// Source is the provenance record.
	Source Source `json:"source"`
	// License is the SPDX id or license name; surfaced so commercial use is an
	// informed choice. Required, so an unknown-license entry cannot slip in unlabeled.
	License string `json:"license"`
	// ParamsB is the parameter count in billions (0 when undisclosed, as for hosted
	// proprietary models).
	ParamsB float64 `json:"paramsB,omitempty"`
	// ContextTokens is the usable context window (0 when unknown).
	ContextTokens int `json:"contextTokens,omitempty"`
	// Capabilities is what the model supports.
	Capabilities Capabilities `json:"capabilities"`
	// Quants are the downloadable quantizations (local models); empty for API models.
	Quants []Quant `json:"quants,omitempty"`
	// Trust is how far the entry has been vetted.
	Trust Trust `json:"trust"`
	// Notes is an optional one-line human note (why it is here, caveats).
	Notes string `json:"notes,omitempty"`
}

// Local reports whether the entry is local weights.
func (m ModelSpec) Local() bool { return m.Kind == KindLocal }

// SmallestQuant returns the smallest-on-disk quantization and whether the model has
// any, so callers can rank a local model by its cheapest footprint.
func (m ModelSpec) SmallestQuant() (Quant, bool) {
	var best Quant
	found := false
	for _, q := range m.Quants {
		if !found || q.SizeBytes < best.SizeBytes {
			best, found = q, true
		}
	}
	return best, found
}

// Catalog is a versioned set of model entries.
type Catalog struct {
	// Version is the catalog schema/content version.
	Version string `json:"version"`
	// Updated is the date the catalog was last revised (ISO 8601), set by the
	// publisher, not at runtime, so the embedded copy is deterministic.
	Updated string `json:"updated"`
	// Models are the entries.
	Models []ModelSpec `json:"models"`
}

//go:embed models.json
var seed []byte

// Load parses and validates the embedded curated catalog. A malformed or invalid
// embedded catalog is a build-time mistake, so it returns a terminal error rather
// than shipping a broken list.
func Load() (Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(seed, &c); err != nil {
		return Catalog{}, fault.Wrap(fault.Terminal, "catalog_decode", err)
	}
	if err := c.Validate(); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

// Validate checks every entry has the provenance and license the catalog promises,
// and that quant formats are known. It is the gate that keeps an unlabeled or unsafe
// entry out of a shipped catalog.
func (c Catalog) Validate() error {
	seen := map[string]bool{}
	for _, m := range c.Models {
		switch {
		case m.ID == "":
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: an entry has no id")
		case seen[m.ID]:
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: duplicate id "+m.ID)
		case m.Name == "":
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: "+m.ID+" has no name")
		case m.License == "":
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: "+m.ID+" has no license")
		case m.Source.Publisher == "" || m.Source.URL == "":
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: "+m.ID+" has no provenance (publisher + url)")
		case m.Kind != KindAPI && m.Kind != KindLocal:
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: "+m.ID+" has an unknown kind "+string(m.Kind))
		case m.Kind == KindLocal && len(m.Quants) == 0:
			return fault.New(fault.Terminal, "catalog_invalid", "catalog: local model "+m.ID+" has no quantizations")
		}
		for _, q := range m.Quants {
			if q.Format != FormatSafetensors && q.Format != FormatGGUF && q.Format != FormatPickle {
				return fault.New(fault.Terminal, "catalog_invalid", "catalog: "+m.ID+" quant "+q.Name+" has an unknown format")
			}
		}
		seen[m.ID] = true
	}
	return nil
}

// Query filters the catalog. The zero Query matches everything.
type Query struct {
	// Kind, when set, restricts to API or local models.
	Kind Kind
	// Capability, when set ("tools", "vision", "reasoning"), keeps models that support
	// it.
	Capability string
	// MaxParamsB, when > 0, keeps models at or below this parameter count (the lever
	// for "what a small box can run"); entries with an undisclosed count are dropped,
	// since fit cannot be claimed for them.
	MaxParamsB float64
	// Publisher, when set, keeps entries from that publisher (case-insensitive).
	Publisher string
	// SafeOnly drops entries whose only quantizations are an unsafe (code-executing)
	// format, so a fit list never steers toward weights that run code on load.
	SafeOnly bool
}

// Find returns the entries matching q, sorted by kind then smallest footprint then id,
// so a list reads stably and the cheapest option surfaces first.
func (c Catalog) Find(q Query) []ModelSpec {
	var out []ModelSpec
	for _, m := range c.Models {
		if !q.matches(m) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

func (q Query) matches(m ModelSpec) bool {
	if q.Kind != "" && m.Kind != q.Kind {
		return false
	}
	if q.Publisher != "" && !strings.EqualFold(q.Publisher, m.Source.Publisher) {
		return false
	}
	if q.MaxParamsB > 0 && (m.ParamsB <= 0 || m.ParamsB > q.MaxParamsB) {
		return false
	}
	switch q.Capability {
	case "tools":
		if !m.Capabilities.Tools {
			return false
		}
	case "vision":
		if !m.Capabilities.Vision {
			return false
		}
	case "reasoning":
		if !m.Capabilities.Reasoning {
			return false
		}
	}
	if q.SafeOnly && m.Local() && !hasSafeQuant(m) {
		return false
	}
	return true
}

// hasSafeQuant reports whether a local model offers at least one code-free format.
func hasSafeQuant(m ModelSpec) bool {
	for _, q := range m.Quants {
		if q.Format.safe() {
			return true
		}
	}
	return false
}

// less orders entries for a stable listing: API models first, then by smallest
// footprint (a local model's cheapest quant; API models have none and tie at zero),
// then by id.
func less(a, b ModelSpec) bool {
	if a.Kind != b.Kind {
		return a.Kind == KindAPI
	}
	sa, _ := a.SmallestQuant()
	sb, _ := b.SmallestQuant()
	if sa.SizeBytes != sb.SizeBytes {
		return sa.SizeBytes < sb.SizeBytes
	}
	return a.ID < b.ID
}
