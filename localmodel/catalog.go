package localmodel

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
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

// Ranked is a [Model] with the [FitPlan] that placed it in a [Host.Rank] ordering.
type Ranked = catalog.Ranked

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
	b := h.detectBox(ctx)
	return Hardware{GPUName: b.GPUName, VRAMBytes: b.VRAMBytes, RAMBytes: b.RAMBytes}
}

// Machine is the memory this machine offers a model: the VRAM free now rather than the
// card's total, because the desktop and other programs already hold some and a model sized
// to the total fails to load, and the system RAM left after the share the OS keeps, which
// is where a mixture-of-experts model's overflow experts go. It is the machine
// `flynn models --fit` judges against, so a host and the command give the same verdicts.
func (h *Host) Machine(ctx context.Context) Machine {
	box := h.detectBox(ctx)
	var mc Machine
	if box.HasGPU() {
		mc.VRAMFreeBytes = box.VRAMBytes
		if free, ok := h.freeVRAM(ctx); ok && free >= 0 && free < box.VRAMBytes {
			mc.VRAMFreeBytes = free
		}
	}
	if box.HasRAM() {
		mc.RAMBytes = cpuInferenceBudget(box.RAMBytes)
	}
	return mc
}

// cpuInferenceBudget is the share of system RAM a model may claim: all but a quarter, or
// all but two gigabytes when that leaves more, since the OS and the programs around the
// model need the memory too.
func cpuInferenceBudget(ramBytes int64) int64 {
	reserve := max(ramBytes/4, 2_000_000_000)
	if reserve >= ramBytes {
		return 0
	}
	return ramBytes - reserve
}

// PlanFit judges the model's smallest quantization against [Host.Machine] at the context
// length and cache precision in s. The cache is counted at that context, memory already in
// use is taken off, and a mixture-of-experts model that does not fit the GPU is also tried
// with its experts in system RAM.
func (h *Host) PlanFit(ctx context.Context, id string, s FitSettings) (FitPlan, error) {
	m, err := h.Model(id)
	if err != nil {
		return FitPlan{Fit: FitUnknown, Placement: PlacementNone}, err
	}
	return m.PlanFit(h.Machine(ctx), s), nil
}

// Fit is the verdict of [Host.PlanFit] at the default settings.
func (h *Host) Fit(ctx context.Context, id string) (Fit, error) {
	p, err := h.PlanFit(ctx, id, FitSettings{})
	return p.Fit, err
}

// Rank lists the local models that match q, strongest first for this machine: those that
// run, comfortable fits before tight ones and larger models before smaller, then those that
// do not. On a machine with a GPU a model that runs only from system RAM ranks last among
// those that run. The order is `flynn models --fit`'s.
func (h *Host) Rank(ctx context.Context, q ModelQuery, s FitSettings) []Ranked {
	return catalog.Rank(h.Models(q), h.Machine(ctx), s)
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
	src, err := h.parse(ref)
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

// parse reads a model reference into a source. Only a [TrustBlessed] entry parses as a
// catalog source; an entry at any other tier is classified by the origin it names, so a
// merged catalog cannot carry a user's entry into the pinned, trusted class by its id.
func (h *Host) parse(ref string) (modelsource.Source, error) {
	if m, err := h.Model(ref); err == nil && m.Unverified() {
		return entrySource(m), nil
	}
	return modelsource.Parse(ref, h.isCatalogID)
}

// isCatalogID reports whether s names a vetted local entry. A failed lookup, a hosted model
// and an entry that is not [TrustBlessed] all answer false, so the test fails closed.
func (h *Host) isCatalogID(s string) bool {
	m, err := h.Model(s)
	return err == nil && m.Local() && !m.Unverified()
}

// publishers returns the distinct publishers the vetted catalog entries name, so a hub
// model from a publisher a curator already vouched for is recognized. An entry that is not
// [TrustBlessed] is left out: a publisher it names is a claim, and counting it would let an
// entry vouch for the next hub model from the same name.
func (h *Host) publishers() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range h.models {
		if m.Unverified() {
			continue
		}
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
