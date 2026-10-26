package localmodel

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/hardware"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/sandbox"
)

// Fit is how a model sits against this machine's memory: [FitFeasible] leaves room for
// the cache and activations, [FitTight] loads with little to spare, [FitOverBudget] does
// not load, and [FitUnknown] means the size or the machine's memory is not known.
type Fit = catalog.Fit

// The Fit values.
const (
	FitFeasible   = catalog.FitFeasible
	FitTight      = catalog.FitTight
	FitOverBudget = catalog.FitOverBudget
	FitUnknown    = catalog.FitUnknown
)

// ModelQuery narrows [Host.Models]. The zero value matches every local model.
type ModelQuery struct {
	// Capability keeps models that support it: "tools", "vision" or "reasoning".
	Capability string
	// MaxParamsB keeps models at or below this many billion parameters. Models that do
	// not disclose a count are dropped.
	MaxParamsB float64
	// Publisher keeps entries from one publisher, ignoring case.
	Publisher string
	// SafeOnly drops models whose only weights use a format that executes code on load.
	SafeOnly bool
}

// Hardware is what was detected about this machine. A zero field means not detected,
// which a reader treats as unknown rather than as no capacity.
type Hardware struct {
	GPUName   string
	VRAMBytes int64
	RAMBytes  int64
}

// Models lists the local models in the catalog that match q.
func (h *Host) Models(q ModelQuery) []Model {
	return cloneModels(catalog.Catalog{Models: h.models}.Find(catalog.Query{
		Kind:       catalog.KindLocal,
		Capability: q.Capability,
		MaxParamsB: q.MaxParamsB,
		Publisher:  q.Publisher,
		SafeOnly:   q.SafeOnly,
	}))
}

// cloneModels copies the models and the slices inside them. A [Model] returned by value
// would otherwise share its Quants backing array with the host's catalog, and a caller
// that edited a digest or a URL through it would change what the host fetches and trusts.
func cloneModels(in []Model) []Model {
	if in == nil {
		return nil
	}
	out := make([]Model, len(in))
	for i, m := range in {
		out[i] = cloneModel(m)
	}
	return out
}

func cloneModel(m Model) Model {
	if m.Quants == nil {
		return m
	}
	qs := make([]Quant, len(m.Quants))
	for i, q := range m.Quants {
		if q.Files != nil {
			q.Files = append([]QuantFile(nil), q.Files...)
		}
		qs[i] = q
	}
	m.Quants = qs
	return m
}

// Model returns the local catalog model with this id.
func (h *Host) Model(id string) (Model, error) {
	id = strings.TrimSpace(id)
	for _, m := range h.models {
		if m.ID != id {
			continue
		}
		if !m.Local() {
			return Model{}, fmt.Errorf("localmodel: %q is a hosted model; there is nothing to run locally", id)
		}
		return cloneModel(m), nil
	}
	return Model{}, fmt.Errorf("localmodel: %q is not in the catalog", id)
}

// Hardware detects this machine's accelerator and memory.
func (h *Host) Hardware(ctx context.Context) Hardware {
	b := hardware.Detect(ctx)
	return Hardware{GPUName: b.GPUName, VRAMBytes: b.VRAMBytes, RAMBytes: b.RAMBytes}
}

// Fit judges the model's smallest quantization against GPU memory when a GPU is present
// and against system memory otherwise, reserving headroom for the cache.
func (h *Host) Fit(ctx context.Context, id string) (Fit, error) {
	m, err := h.Model(id)
	if err != nil {
		return FitUnknown, err
	}
	hw := h.Hardware(ctx)
	budget := hw.RAMBytes
	if hw.VRAMBytes > 0 {
		budget = hw.VRAMBytes
	}
	return m.FitFor(budget), nil
}

// Assessment is the outcome of classifying a model reference: how far its source is
// trusted, the isolation that trust needs, and whether this host provides it.
type Assessment struct {
	// Ref is the reference as given, with any URL reduced to its host: a reference can
	// carry a credential, and an Assessment is built to be displayed.
	Ref string
	// Catalog is true for a vetted catalog entry, the only kind [Host.Serve] runs.
	Catalog bool
	// Trust is "trusted", "semi-trusted" or "untrusted", with Reason in plain language.
	Trust  string
	Reason string
	// Required and Provided name the isolation the trust needs and the sandbox gives.
	Required string
	Provided string
	// Admitted is false when Provided is weaker than Required; Refusal then says so.
	Admitted bool
	Refusal  string
	// Risk is the plain-language summary of trust, isolation, integrity and network
	// posture, one line each, for showing to a person before anything runs.
	Risk []string
	// Risky is true when the summary calls for explicit consent.
	Risky bool
}

// Classify parses a model reference, refuses a weight format that executes code on load,
// records the source's provenance, and reports whether this host's sandbox isolates it
// enough. It has no other effect. A refusal by isolation is reported in the Assessment,
// not as an error, so a caller can explain it.
func (h *Host) Classify(ref string) (Assessment, error) {
	_, _, a, err := h.assess(ref)
	return a, h.fail(err)
}

// assess is Classify with the parsed source and classification a serve path needs.
func (h *Host) assess(ref string) (modelsource.Source, modelsource.Classification, Assessment, error) {
	src, err := modelsource.Parse(ref, h.isCatalogID)
	if err != nil {
		return src, modelsource.Classification{}, Assessment{}, err
	}
	class := modelsource.Classify(src, func(owner string) bool {
		return modelsource.KnownPublisher(owner, h.publishers()...)
	})
	if name := sourceFileName(src); name != "" {
		if err := modelsource.CheckRunnableFormat(name); err != nil {
			return src, class, Assessment{}, err
		}
	}
	_ = h.ledger.Record(modelsource.Provenance{Key: src.Key(), Raw: src.Raw, Trust: class.Trust.String()})

	integ := modelsource.IntegrityUnverified
	if src.Kind == modelsource.KindCatalog {
		integ = modelsource.IntegrityPinned
	} else if d, ok, _ := h.ledger.PinnedDigest(src.Key()); ok && d != "" {
		integ = modelsource.IntegrityTOFU
	}
	rs := modelsource.DescribeRisk(src, class, integ)
	need, have := sandbox.Required(class.Trust), sandbox.ContainmentOf(h.sb)
	a := Assessment{
		Ref:      redactText(ref),
		Catalog:  src.Kind == modelsource.KindCatalog,
		Trust:    class.Trust.String(),
		Reason:   class.Reason,
		Required: need.String(),
		Provided: have.String(),
		Admitted: sandbox.Admit(h.sb, class.Trust) == nil,
		Risk:     redactLines(rs.Lines()),
		Risky:    rs.Risky(),
	}
	if !a.Admitted {
		a.Refusal = fmt.Sprintf("%s work needs %s isolation, but this host provides only %s; refusing to run it unsafely", a.Trust, a.Required, a.Provided)
	}
	return src, class, a, nil
}

func (h *Host) isCatalogID(s string) bool {
	m, err := h.Model(s)
	return err == nil && m.Local()
}

// publishers returns the distinct publishers the catalog names, so a hub model from a
// publisher a curator already vouched for is recognized.
func (h *Host) publishers() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range h.models {
		if p := m.Source.Publisher; p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// sourceFileName returns the weights file a reference names, for the format guard, or
// empty when the concrete file is not known from the reference alone.
func sourceFileName(src modelsource.Source) string {
	switch src.Kind {
	case modelsource.KindHuggingFace:
		return src.File
	case modelsource.KindURL:
		return src.URL
	case modelsource.KindFile:
		return src.Path
	default:
		return ""
	}
}
