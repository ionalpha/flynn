package catalog

import "sort"

// KVType is the precision the runtime stores the key/value cache in. A quantized cache
// roughly halves the context cost, which is what lets a longer window fit a small card.
type KVType string

const (
	// KVF16 is the runtime default: two bytes per cached element.
	KVF16 KVType = "f16"
	// KVQ8 is llama.cpp's q8_0 cache type: 34 bytes per 32 elements.
	KVQ8 KVType = "q8_0"
)

// Valid reports whether k is a cache type the estimate knows the size of.
func (k KVType) Valid() bool { return k == KVF16 || k == KVQ8 }

// bytesPerElem is the storage cost of one cached element. An unknown type is costed as
// f16, the larger of the two, so a typo can only make the estimate more cautious.
func (k KVType) bytesPerElem() float64 {
	if k == KVQ8 {
		return 34.0 / 32.0
	}
	return 2
}

const (
	// DefaultContextTokens is the window a fit estimate assumes when the caller does not
	// name one: enough for an agent loop with tool output, without charging every model
	// for its full advertised window (262144 tokens is not what anyone runs on a laptop).
	DefaultContextTokens = 32768

	// runtimeOverheadBytes is what the runtime itself holds beyond weights and cache: the
	// CUDA context, the compute buffers, and the fixed recurrent state of a hybrid model.
	// It is a flat allowance because none of these scale with the context length.
	runtimeOverheadBytes = 600_000_000

	// slackPercent is the share of a memory pool kept unplanned. A plan that fills the
	// pool to the byte is tight, because the allocator fragments and the desktop grows.
	slackPercent = 5

	// gpuToRAMBandwidth is how much faster the GPU reads its own memory than it reads
	// system RAM across the bus: a laptop GPU's memory runs near 400 GB/s against about
	// 50 GB/s for dual-channel DDR5. It sets the speed estimate for a split, and is an
	// assumption about typical hardware, not something the probe measures.
	gpuToRAMBandwidth = 8.0

	// legacyHeadroomDenom sizes the cache of a model with no [Arch] as a fifth of its
	// weights, the allowance the catalog used before it carried architecture data.
	legacyHeadroomDenom = 5
)

// Machine is the memory a fit is judged against, after what is already in use has been
// taken off. Both fields are what a model may claim, not what the hardware has.
type Machine struct {
	// VRAMFreeBytes is the GPU memory that is free now: total minus what the desktop and
	// other processes hold. Zero means there is no usable GPU.
	VRAMFreeBytes int64
	// RAMBytes is the system memory a model may use, after the share the OS and other
	// programs need. On a GPU machine it is where moved-out experts live; with no GPU it
	// is the whole budget.
	RAMBytes int64
}

// FitSettings is the workload a fit is judged for. Memory use depends on how long a
// context the user will run, so the same model fits at 8k and not at 128k.
type FitSettings struct {
	// ContextTokens is the window to size the cache for; zero means [DefaultContextTokens].
	// A model's own limit caps it.
	ContextTokens int
	// KV is the cache precision; empty means [KVF16].
	KV KVType
}

// Placement is where a plan puts the model.
type Placement string

const (
	// PlacementAPI is a hosted model: nothing is loaded locally.
	PlacementAPI Placement = "api"
	// PlacementGPU keeps weights and cache in VRAM.
	PlacementGPU Placement = "gpu"
	// PlacementSplit keeps the dense part and the cache in VRAM and puts the routed
	// experts that do not fit there in system RAM. Only a mixture-of-experts model can
	// do this, because only part of the weight is read per token.
	PlacementSplit Placement = "gpu+ram"
	// PlacementCPU runs from system RAM alone.
	PlacementCPU Placement = "cpu"
	// PlacementNone means no placement could be made, or there was nothing to judge.
	PlacementNone Placement = "-"
)

// FitPlan is a fit verdict with the arithmetic behind it, so a caller can show why a
// model landed where it did.
type FitPlan struct {
	// Fit is the verdict for the chosen placement.
	Fit Fit
	// Placement is where the model would run.
	Placement Placement
	// Quant is the quantization judged: the smallest one the entry offers.
	Quant string
	// ContextTokens is the window the cache is sized for, after the model's own limit.
	ContextTokens int
	// KV is the cache precision used.
	KV KVType
	// WeightBytes, KVBytes and OverheadBytes sum to [FitPlan.NeedBytes].
	WeightBytes, KVBytes, OverheadBytes int64
	// KVEstimated is true when the entry has no [Arch], so KVBytes is a flat fraction of
	// the weights rather than computed from the network's shape.
	KVEstimated bool
	// GPUBytes and RAMBytes are how NeedBytes divides between VRAM and system RAM.
	GPUBytes, RAMBytes int64
	// ExpertsOnCPU is the share of the routed experts held in RAM, for a split.
	ExpertsOnCPU float64
	// RelativeSpeed estimates a split's generation speed as a share of the same model
	// fully on the GPU (1 means no slowdown). Zero when the placement is not a split.
	RelativeSpeed float64
}

// NeedBytes is the total memory the plan claims across both pools.
func (p FitPlan) NeedBytes() int64 { return p.WeightBytes + p.KVBytes + p.OverheadBytes }

// KVCacheBytes is the key/value cache for ctx tokens: two tensors (K and V) per
// attention layer, each KVHeads x HeadDim wide per token.
func (a Arch) KVCacheBytes(ctx int, kv KVType) int64 {
	elems := 2 * float64(a.AttentionLayers) * float64(a.KVHeads) * float64(a.HeadDim) * float64(ctx)
	return int64(elems * kv.bytesPerElem())
}

// PlanFit judges the model's smallest quantization against the machine at the chosen
// context length. Cache, runtime overhead and weights are all counted, and an MoE model
// that does not fit the GPU is also tried with its experts spread into system RAM. When
// several placements work, the best verdict wins and a GPU-only run wins a tie, so the
// split is reported only when it is the way the model runs at all, or runs better.
//
// A larger machine never gets a worse verdict, because each placement's verdict only
// improves with memory and the best of them is taken.
func (m ModelSpec) PlanFit(mc Machine, s FitSettings) FitPlan {
	if !m.Local() {
		return FitPlan{Fit: FitFeasible, Placement: PlacementAPI}
	}
	q, ok := m.SmallestQuant()
	if !ok || q.SizeBytes <= 0 || (mc.VRAMFreeBytes <= 0 && mc.RAMBytes <= 0) {
		return FitPlan{Fit: FitUnknown, Placement: PlacementNone}
	}

	ctx, kv := s.ContextTokens, s.KV
	if ctx <= 0 {
		ctx = DefaultContextTokens
	}
	if m.ContextTokens > 0 && ctx > m.ContextTokens {
		ctx = m.ContextTokens
	}
	if !kv.Valid() {
		kv = KVF16
	}
	p := FitPlan{
		Placement: PlacementNone, Fit: FitOverBudget, Quant: q.Name, ContextTokens: ctx, KV: kv,
		WeightBytes: q.SizeBytes, OverheadBytes: runtimeOverheadBytes,
	}
	if m.Arch != nil {
		p.KVBytes = m.Arch.KVCacheBytes(ctx, kv)
	} else {
		p.KVBytes, p.KVEstimated = q.SizeBytes/legacyHeadroomDenom, true
	}

	// Listed in order of preference, so a tie goes to the faster placement. The CPU is an
	// option on a GPU machine too: without it a card with a few bytes free would rank
	// below no card at all.
	var options []FitPlan
	if mc.VRAMFreeBytes > 0 {
		options = append(options, p.onGPU(mc))
		if m.Arch != nil && m.Arch.MoE() && m.ParamsB > 0 {
			options = append(options, p.splitAcrossRAM(m, mc))
		}
	}
	options = append(options, p.onCPU(mc))
	best := options[0]
	for _, o := range options[1:] {
		if verdictRank(o.Fit) > verdictRank(best.Fit) {
			best = o
		}
	}
	return best
}

// verdictRank orders verdicts so placements can be compared.
func verdictRank(f Fit) int {
	switch f {
	case FitFeasible:
		return 2
	case FitTight:
		return 1
	default:
		return 0
	}
}

// within grades need against a pool: feasible with the slack kept free, tight when it
// fits only by using the slack, over-budget when it does not fit.
func within(need, pool int64) Fit {
	switch {
	case need <= pool-pool*slackPercent/100:
		return FitFeasible
	case need <= pool:
		return FitTight
	default:
		return FitOverBudget
	}
}

// onGPU places everything in VRAM.
func (p FitPlan) onGPU(mc Machine) FitPlan {
	p.Placement, p.GPUBytes, p.RAMBytes = PlacementGPU, p.NeedBytes(), 0
	p.Fit = within(p.NeedBytes(), mc.VRAMFreeBytes)
	return p
}

// onCPU places everything in system RAM.
func (p FitPlan) onCPU(mc Machine) FitPlan {
	p.Placement, p.GPUBytes, p.RAMBytes = PlacementCPU, 0, p.NeedBytes()
	p.Fit = within(p.NeedBytes(), mc.RAMBytes)
	return p
}

// splitAcrossRAM keeps the dense weights, the cache and the runtime in VRAM and fills
// what VRAM is left with experts, so as few as possible are read across the bus. The
// cache stays on the GPU because every token touches all of it, while a token touches
// only ExpertsPerToken of the experts.
func (p FitPlan) splitAcrossRAM(m ModelSpec, mc Machine) FitPlan {
	a := m.Arch
	expertFrac := a.ExpertParamsB / m.ParamsB
	if expertFrac > 1 {
		expertFrac = 1
	}
	expertBytes := int64(float64(p.WeightBytes) * expertFrac)
	denseBytes := p.WeightBytes - expertBytes

	p.Placement = PlacementSplit
	base := denseBytes + p.KVBytes + p.OverheadBytes
	room := mc.VRAMFreeBytes - mc.VRAMFreeBytes*slackPercent/100 - base
	if room < 0 {
		p.Fit, p.GPUBytes, p.RAMBytes = FitOverBudget, base, expertBytes
		p.ExpertsOnCPU = 1
		return p
	}
	onGPU := min(room, expertBytes)
	p.GPUBytes, p.RAMBytes = base+onGPU, expertBytes-onGPU
	if expertBytes > 0 {
		p.ExpertsOnCPU = float64(p.RAMBytes) / float64(expertBytes)
	}
	if p.RAMBytes == 0 {
		// The experts all fit beside the cache once the slack is kept free, which a
		// plain GPU placement at the same size only reports as tight.
		p.Placement, p.Fit = PlacementGPU, FitFeasible
		return p
	}
	p.Fit = within(p.RAMBytes, mc.RAMBytes)

	// Each token reads the dense part plus ExpertsPerToken of Experts. The share of that
	// expert read that comes from RAM is the share of experts held there, and a read from
	// RAM costs gpuToRAMBandwidth times a read from VRAM.
	active := float64(expertBytes) * float64(a.ExpertsPerToken) / float64(a.Experts)
	allGPU := float64(denseBytes) + active
	p.RelativeSpeed = allGPU / (float64(denseBytes) + active*(1-p.ExpertsOnCPU) + active*p.ExpertsOnCPU*gpuToRAMBandwidth)
	return p
}

// Ranked is a model with the plan that decided where it sits in a ranking.
type Ranked struct {
	Model ModelSpec
	Plan  FitPlan
	// CPUFallback is true when the machine has a GPU and the model runs only from system
	// RAM. It runs, but at a fraction of GPU speed, so it ranks below every model that
	// uses the GPU and is not offered as a recommendation.
	CPUFallback bool
}

// Rank orders models for a machine, strongest runnable first: local models that run
// (feasible before tight, then larger parameter count first), then hosted models, then
// local models that do not run. On a machine with a GPU, a model that can only run from
// system RAM ranks last among those that run. Ties break on id so the order is stable. A split is
// ranked like any other placement, since its row carries the label and the slowdown.
func Rank(models []ModelSpec, mc Machine, s FitSettings) []Ranked {
	out := make([]Ranked, len(models))
	for i, m := range models {
		p := m.PlanFit(mc, s)
		out[i] = Ranked{Model: m, Plan: p, CPUFallback: mc.VRAMFreeBytes > 0 && p.Placement == PlacementCPU}
	}
	group := func(r Ranked) int {
		switch {
		case !r.Model.Local():
			return 1
		case r.Plan.Fit.Runnable():
			return 0
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ga, gb := group(a), group(b); ga != gb {
			return ga < gb
		}
		if a.CPUFallback != b.CPUFallback {
			return !a.CPUFallback
		}
		if group(a) == 0 && a.Plan.Fit != b.Plan.Fit {
			return verdictRank(a.Plan.Fit) > verdictRank(b.Plan.Fit)
		}
		if a.Model.ParamsB != b.Model.ParamsB {
			return a.Model.ParamsB > b.Model.ParamsB
		}
		return a.Model.ID < b.Model.ID
	})
	return out
}

// Top returns the first local model in a ranking that fits comfortably, optionally
// skipping split placements, so a caller can name both the strongest model the box runs
// and the strongest that stays wholly on one pool of memory.
func Top(ranked []Ranked, includeSplit bool) (Ranked, bool) {
	for _, r := range ranked {
		if !r.Model.Local() || r.Plan.Fit != FitFeasible || r.CPUFallback {
			continue
		}
		if r.Plan.Placement == PlacementSplit && !includeSplit {
			continue
		}
		return r, true
	}
	return Ranked{}, false
}
