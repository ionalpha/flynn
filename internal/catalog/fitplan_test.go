package catalog

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

const gb = 1_000_000_000

func mustModel(t testing.TB, id string) ModelSpec {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("catalog has no %s", id)
	return ModelSpec{}
}

func localModels(t testing.TB) []ModelSpec {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c.Find(Query{Kind: KindLocal})
}

// TestKVCacheBytes pins the cache formula to a number worked by hand from the
// publisher's config: Qwen3.6-27B has 16 full-attention layers with 4 KV heads of
// width 256, so a token costs 2 x 16 x 4 x 256 x 2 bytes = 65536 bytes at f16.
func TestKVCacheBytes(t *testing.T) {
	a := mustModel(t, "qwen3.6:27b").Arch
	if a == nil {
		t.Fatal("qwen3.6:27b has no arch")
	}
	if got, want := a.KVCacheBytes(1000, KVF16), int64(65_536_000); got != want {
		t.Fatalf("f16 cache for 1000 tokens = %d, want %d", got, want)
	}
	// q8_0 is 34 bytes per 32 elements, 17/32 of the f16 size.
	if got, want := a.KVCacheBytes(1000, KVQ8), int64(34_816_000); got != want {
		t.Fatalf("q8_0 cache for 1000 tokens = %d, want %d", got, want)
	}
}

// boxes are real machines from the field test and the usual neighbours of it. VRAM is
// the free figure (total minus what the desktop holds); RAM is what a model may claim.
var boxes = map[string]Machine{
	"16GB laptop GPU, 16GB RAM": {VRAMFreeBytes: 16_000_000_000, RAMBytes: 12 * gb},
	"12GB GPU, 64GB RAM":        {VRAMFreeBytes: 11_000_000_000, RAMBytes: 48 * gb},
	"24GB GPU, 32GB RAM":        {VRAMFreeBytes: 23_000_000_000, RAMBytes: 24 * gb},
	"CPU only, 16GB RAM":        {RAMBytes: 12 * gb},
}

// TestRankRealBoxes is the acceptance table: what each box ranks first, and where.
func TestRankRealBoxes(t *testing.T) {
	models := localModels(t)
	cases := []struct {
		box        string
		top        string
		placement  Placement
		topNoSplit string
	}{
		// 20.4GB of weights cannot sit in 16GB, but the experts can: the dense 1.6GB and the
		// cache stay on the card and the rest of the experts go to RAM. The best model that
		// stays wholly on a 16GB card is the 21B gpt-oss mixture of experts (12.1GB of weights); on
		// 12GB it is the 12B Gemma, whose few global-attention layers keep its cache small.
		{"16GB laptop GPU, 16GB RAM", "qwen3.6:35b-a3b", PlacementSplit, "gpt-oss-20b"},
		{"12GB GPU, 64GB RAM", "qwen3.6:35b-a3b", PlacementSplit, "gemma4:12b"},
		// The whole 35B mixture of experts (20.4GB plus cache and runtime) fits 23GB, and
		// ranks above the dense 27B on parameter count.
		{"24GB GPU, 32GB RAM", "qwen3.6:35b-a3b", PlacementGPU, "qwen3.6:35b-a3b"},
		// With 12GB of RAM to claim, the 35B and the 21B do not fit and the 12B Gemma is the
		// largest model that does.
		{"CPU only, 16GB RAM", "gemma4:12b", PlacementCPU, "gemma4:12b"},
	}
	for _, tc := range cases {
		t.Run(tc.box, func(t *testing.T) {
			ranked := Rank(models, boxes[tc.box], FitSettings{})
			top, ok := Top(ranked, true)
			if !ok || top.Model.ID != tc.top || top.Plan.Placement != tc.placement {
				t.Fatalf("top = %s on %s (%v), want %s on %s", top.Model.ID, top.Plan.Placement, ok, tc.top, tc.placement)
			}
			alt, ok := Top(ranked, false)
			if !ok || alt.Model.ID != tc.topNoSplit {
				t.Fatalf("top without split = %s (%v), want %s", alt.Model.ID, ok, tc.topNoSplit)
			}
		})
	}
}

// TestSplitOnTheFieldTestBox checks the arithmetic of the case the task came from, so a
// change to the model shows up as a changed number rather than a changed label.
func TestSplitOnTheFieldTestBox(t *testing.T) {
	m := mustModel(t, "qwen3.6:35b-a3b")
	p := m.PlanFit(boxes["16GB laptop GPU, 16GB RAM"], FitSettings{})

	if p.Placement != PlacementSplit || p.Fit != FitFeasible {
		t.Fatalf("placement %s fit %s, want a feasible split", p.Placement, p.Fit)
	}
	if p.ContextTokens != DefaultContextTokens || p.KV != KVF16 {
		t.Fatalf("context %d kv %s, want the defaults", p.ContextTokens, p.KV)
	}
	// 10 attention layers x 2 KV heads x 256 wide x 2 tensors x 2 bytes x 32768 tokens.
	if want := int64(671_088_640); p.KVBytes != want {
		t.Fatalf("KV = %d, want %d", p.KVBytes, want)
	}
	if p.KVEstimated {
		t.Fatal("a model with arch data must not report an estimated cache")
	}
	if p.GPUBytes+p.RAMBytes != p.NeedBytes() {
		t.Fatalf("GPU %d + RAM %d != need %d", p.GPUBytes, p.RAMBytes, p.NeedBytes())
	}
	if p.GPUBytes > 16*gb-16*gb*slackPercent/100 {
		t.Fatalf("GPU share %d overruns the card less its slack", p.GPUBytes)
	}
	if p.ExpertsOnCPU <= 0 || p.ExpertsOnCPU >= 0.5 {
		t.Fatalf("experts on CPU = %.2f, want a minority of them", p.ExpertsOnCPU)
	}
	// Few experts run per token and most sit on the card, so the split stays usable but is
	// visibly slower than all-GPU.
	if p.RelativeSpeed < 0.4 || p.RelativeSpeed >= 1 {
		t.Fatalf("relative speed %.2f, want between 0.4 and 1", p.RelativeSpeed)
	}
}

// TestDenseModelDoesNotSplit: only a mixture of experts can be spread over GPU and RAM.
// A dense model larger than the card is over budget while RAM is short, and with RAM to
// hold all of it runs from RAM alone, never as a split.
func TestDenseModelDoesNotSplit(t *testing.T) {
	m := mustModel(t, "qwen3.6:27b")
	p := m.PlanFit(Machine{VRAMFreeBytes: 16 * gb, RAMBytes: 12 * gb}, FitSettings{})
	if p.Fit != FitOverBudget {
		t.Fatalf("got %s on %s, want over-budget", p.Fit, p.Placement)
	}
	p = m.PlanFit(Machine{VRAMFreeBytes: 16 * gb, RAMBytes: 200 * gb}, FitSettings{})
	if p.Fit != FitFeasible || p.Placement != PlacementCPU {
		t.Fatalf("got %s on %s, want feasible on cpu", p.Fit, p.Placement)
	}
}

// TestCPUFallbackIsNotRecommended: on a machine with a GPU, a model that fits only in RAM
// is ranked after the ones that use the GPU and never named as the top choice.
func TestCPUFallbackIsNotRecommended(t *testing.T) {
	mc := Machine{VRAMFreeBytes: 11 * gb, RAMBytes: 48 * gb}
	ranked := Rank(localModels(t), mc, FitSettings{})
	var cpu, gpu int
	for i, r := range ranked {
		switch {
		case r.CPUFallback && cpu == 0:
			cpu = i + 1
		case !r.CPUFallback && r.Plan.Fit.Runnable() && r.Model.Local():
			gpu = i + 1
		}
	}
	if cpu == 0 {
		t.Fatal("the dense 27B should run from RAM on this box")
	}
	if cpu < gpu {
		t.Fatalf("a CPU fallback ranked at %d, above a GPU model at %d", cpu, gpu)
	}
	if top, _ := Top(ranked, true); top.CPUFallback {
		t.Fatalf("top = %s from RAM", top.Model.ID)
	}
}

// TestVRAMInUseChangesTheVerdict: the same card with a heavier desktop loses a model.
func TestVRAMInUseChangesTheVerdict(t *testing.T) {
	m := mustModel(t, "qwen3.5:9b")
	total := int64(8 * gb)
	idle := m.PlanFit(Machine{VRAMFreeBytes: total}, FitSettings{})
	busy := m.PlanFit(Machine{VRAMFreeBytes: total - 2*gb}, FitSettings{})
	if idle.Fit != FitFeasible {
		t.Fatalf("9B on an idle 8GB card = %s, want feasible", idle.Fit)
	}
	if busy.Fit == FitFeasible {
		t.Fatalf("9B with 2GB already in use = %s, want a worse verdict", busy.Fit)
	}
}

// TestContextLengthChangesTheVerdict: a longer window costs cache, and a quantized cache
// buys some of it back.
func TestContextLengthChangesTheVerdict(t *testing.T) {
	m := mustModel(t, "qwen3.6:27b")
	mc := Machine{VRAMFreeBytes: 22 * gb}
	short := m.PlanFit(mc, FitSettings{ContextTokens: 8192})
	long := m.PlanFit(mc, FitSettings{ContextTokens: 131072})
	if short.Fit != FitFeasible || long.Fit != FitOverBudget {
		t.Fatalf("8k = %s, 128k = %s; want feasible then over-budget", short.Fit, long.Fit)
	}
	q8 := m.PlanFit(mc, FitSettings{ContextTokens: 131072, KV: KVQ8})
	if q8.KVBytes >= long.KVBytes || q8.KVBytes <= long.KVBytes/2 {
		t.Fatalf("q8 cache %d, f16 cache %d: want slightly over half", q8.KVBytes, long.KVBytes)
	}
}

// TestContextIsCappedAtTheModelWindow: asking for more than the model supports is sized
// at what it supports.
func TestContextIsCappedAtTheModelWindow(t *testing.T) {
	m := mustModel(t, "ollama:qwen2.5-coder:1.5b")
	p := m.PlanFit(Machine{VRAMFreeBytes: 8 * gb}, FitSettings{ContextTokens: 1 << 20})
	if p.ContextTokens != m.ContextTokens {
		t.Fatalf("context = %d, want the model's %d", p.ContextTokens, m.ContextTokens)
	}
}

// TestNoArchFallsBackToAFlatAllowance: an entry without architecture data is judged as
// before, and the plan says its cache is an estimate.
func TestNoArchFallsBackToAFlatAllowance(t *testing.T) {
	m := ModelSpec{Kind: KindLocal, ParamsB: 7, Quants: []Quant{{Name: "Q4_K_M", SizeBytes: 5 * gb}}}
	p := m.PlanFit(Machine{VRAMFreeBytes: 24 * gb}, FitSettings{})
	if !p.KVEstimated || p.KVBytes != gb || p.Fit != FitFeasible {
		t.Fatalf("plan = %+v, want an estimated 1GB cache and a feasible fit", p)
	}
}

func TestPlanFitUnknownAndAPI(t *testing.T) {
	if p := (ModelSpec{Kind: KindAPI}).PlanFit(Machine{}, FitSettings{}); p.Fit != FitFeasible || p.Placement != PlacementAPI {
		t.Fatalf("api plan = %+v", p)
	}
	local := mustModel(t, "qwen3.5:9b")
	if p := local.PlanFit(Machine{}, FitSettings{}); p.Fit != FitUnknown {
		t.Fatalf("no memory known = %s, want unknown", p.Fit)
	}
	if p := (ModelSpec{Kind: KindLocal}).PlanFit(Machine{RAMBytes: gb}, FitSettings{}); p.Fit != FitUnknown {
		t.Fatalf("no quant = %s, want unknown", p.Fit)
	}
	if KVType("bogus").Valid() {
		t.Fatal("an unknown cache type must not validate")
	}
	if got := KVType("bogus").bytesPerElem(); got != 2 {
		t.Fatalf("unknown cache type costs %v, want the f16 cost", got)
	}
}

// TestSplitWithRAMToSpare: when the experts all fit beside the cache the plan is plain
// GPU, not a split with nothing in RAM.
func TestSplitWithRAMToSpare(t *testing.T) {
	m := mustModel(t, "qwen3.6:35b-a3b")
	// Need is 21.7GB, so 22GB is a tight GPU fit and 22.5GB leaves the slack free.
	p := m.PlanFit(Machine{VRAMFreeBytes: 22_900_000_000, RAMBytes: 8 * gb}, FitSettings{})
	if p.Placement != PlacementGPU || p.Fit != FitFeasible || p.RAMBytes != 0 {
		t.Fatalf("plan = %+v, want feasible on gpu", p)
	}
}

// TestSplitOverBudgetCases covers the two ways a split fails: the dense part and cache do
// not fit the card, or the leftover experts do not fit RAM.
func TestSplitOverBudgetCases(t *testing.T) {
	m := mustModel(t, "qwen3.6:35b-a3b")
	small := Machine{VRAMFreeBytes: 2 * gb, RAMBytes: 4 * gb}
	base := m.PlanFit(small, FitSettings{})
	if base.Fit != FitOverBudget {
		t.Fatalf("a 2GB card = %+v, want over-budget", base)
	}
	// The split alone reports that the dense part and cache do not fit the card.
	if p := base.splitAcrossRAM(m, small); p.Fit != FitOverBudget || p.ExpertsOnCPU != 1 {
		t.Fatalf("split on a 2GB card = %+v, want over-budget with every expert on the CPU", p)
	}
	if p := m.PlanFit(Machine{VRAMFreeBytes: 8 * gb, RAMBytes: 2 * gb}, FitSettings{}); p.Fit != FitOverBudget {
		t.Fatalf("8GB card and 2GB RAM = %+v, want over-budget", p)
	}
	// A split that uses RAM beyond its slack is tight, not feasible.
	if p := m.PlanFit(Machine{VRAMFreeBytes: 8 * gb, RAMBytes: 14_500_000_000}, FitSettings{}); p.Fit != FitTight {
		t.Fatalf("a split that fills RAM = %+v, want tight", p)
	}
}

// TestSplitClampsExpertShareToTheWholeModel: [Catalog.Validate] rejects an entry whose
// experts outweigh the model, but a caller can hand [ModelSpec.PlanFit] any spec. The split
// must not then count more expert bytes than the file holds.
func TestSplitClampsExpertShareToTheWholeModel(t *testing.T) {
	m := mustModel(t, "qwen3.6:35b-a3b")
	a := *m.Arch
	a.ExpertParamsB = m.ParamsB * 2
	m.Arch = &a
	p := m.PlanFit(Machine{VRAMFreeBytes: 8 * gb, RAMBytes: 40 * gb}, FitSettings{})
	if p.Placement != PlacementSplit {
		t.Fatalf("plan = %+v, want a split", p)
	}
	if p.RAMBytes > p.WeightBytes {
		t.Fatalf("RAM holds %d bytes of experts, more than the %d-byte file", p.RAMBytes, p.WeightBytes)
	}
}

func TestRankOrder(t *testing.T) {
	models := append(localModels(t), ModelSpec{ID: "api:x", Kind: KindAPI})
	ranked := Rank(models, boxes["16GB laptop GPU, 16GB RAM"], FitSettings{})
	var sawAPI, sawOver bool
	for _, r := range ranked {
		switch {
		case !r.Model.Local():
			sawAPI = true
			if sawOver {
				t.Fatalf("API model %s ranked after a local model that does not run", r.Model.ID)
			}
		case !r.Plan.Fit.Runnable():
			sawOver = true
		case sawAPI || sawOver:
			t.Fatalf("runnable %s ranked after a model that is not runnable", r.Model.ID)
		}
	}
	if !sawAPI || !sawOver {
		t.Fatal("the fixture should include an API model and a model that does not fit")
	}
	if _, ok := Top(nil, true); ok {
		t.Fatal("an empty ranking has no top")
	}
}

// plainLocal is a local model with no architecture data, sized so a test can place it
// exactly: the flat cache allowance is a fifth of the weights.
func plainLocal(id string, paramsB float64, weights int64) ModelSpec {
	return ModelSpec{
		ID: id, Kind: KindLocal, ParamsB: paramsB,
		Quants: []Quant{{Name: "Q4_K_M", Format: "gguf", SizeBytes: weights}},
	}
}

// TestFeasibleRanksBeforeTightAndTopSkipsTight: a smaller model with room to spare is
// listed ahead of a larger one that fits only by using the slack, and neither a tight model
// nor a hosted one is named as the recommendation.
func TestFeasibleRanksBeforeTightAndTopSkipsTight(t *testing.T) {
	mc := Machine{VRAMFreeBytes: 10_000_000_000}
	big := plainLocal("big", 30, 7_800_000_000)
	small := plainLocal("small", 7, 4_000_000_000)
	if p := big.PlanFit(mc, FitSettings{}); p.Fit != FitTight {
		t.Fatalf("fixture: big = %s, want tight", p.Fit)
	}
	if p := small.PlanFit(mc, FitSettings{}); p.Fit != FitFeasible {
		t.Fatalf("fixture: small = %s, want feasible", p.Fit)
	}

	ranked := Rank([]ModelSpec{big, small}, mc, FitSettings{})
	if ranked[0].Model.ID != "small" || ranked[1].Model.ID != "big" {
		t.Fatalf("order = %s, %s; want small then big", ranked[0].Model.ID, ranked[1].Model.ID)
	}
	if top, ok := Top(ranked, true); !ok || top.Model.ID != "small" {
		t.Fatalf("top = %s (%v), want small", top.Model.ID, ok)
	}

	onlyTight := Rank([]ModelSpec{big, {ID: "api:x", Kind: KindAPI}}, mc, FitSettings{})
	if top, ok := Top(onlyTight, true); ok {
		t.Fatalf("top = %s, want none when nothing fits comfortably", top.Model.ID)
	}
}

// TestPlanFitMonotonic is the property that makes the verdict trustworthy: more VRAM or
// more RAM never makes any catalog model fit worse, whichever placement wins.
func TestPlanFitMonotonic(t *testing.T) {
	models := localModels(t)
	rapid.Check(t, func(rt *rapid.T) {
		m := rapid.SampledFrom(models).Draw(rt, "model")
		vram := rapid.Int64Range(0, 80*gb).Draw(rt, "vram")
		ram := rapid.Int64Range(0, 256*gb).Draw(rt, "ram")
		dv := rapid.Int64Range(0, 40*gb).Draw(rt, "dvram")
		dr := rapid.Int64Range(0, 128*gb).Draw(rt, "dram")
		ctx := rapid.IntRange(1, 300_000).Draw(rt, "ctx")
		s := FitSettings{ContextTokens: ctx}
		less, more := m.PlanFit(Machine{vram, ram}, s), m.PlanFit(Machine{vram + dv, ram + dr}, s)
		if less.Fit == FitUnknown || more.Fit == FitUnknown {
			return
		}
		if verdictRank(more.Fit) < verdictRank(less.Fit) {
			rt.Fatalf("%s: (%d,%d)->%s but (%d,%d)->%s", m.ID, vram, ram, less.Fit, vram+dv, ram+dr, more.Fit)
		}
	})
}

// TestPlanFitMemoryIsConserved: whatever the placement, the two pools together hold
// exactly what the plan needs, and a split's speed is a share of full speed.
func TestPlanFitMemoryIsConserved(t *testing.T) {
	models := localModels(t)
	rapid.Check(t, func(rt *rapid.T) {
		m := rapid.SampledFrom(models).Draw(rt, "model")
		mc := Machine{
			VRAMFreeBytes: rapid.Int64Range(1, 80*gb).Draw(rt, "vram"),
			RAMBytes:      rapid.Int64Range(0, 256*gb).Draw(rt, "ram"),
		}
		p := m.PlanFit(mc, FitSettings{ContextTokens: rapid.IntRange(1, 300_000).Draw(rt, "ctx")})
		if p.Fit == FitOverBudget && p.Placement == PlacementSplit {
			return // an over-budget split reports the base it could not place
		}
		if p.GPUBytes+p.RAMBytes != p.NeedBytes() {
			rt.Fatalf("%s: %d + %d != %d", m.ID, p.GPUBytes, p.RAMBytes, p.NeedBytes())
		}
		if p.Placement == PlacementSplit && (p.RelativeSpeed <= 0 || p.RelativeSpeed > 1 || math.IsNaN(p.RelativeSpeed)) {
			rt.Fatalf("%s: split speed %v out of (0,1]", m.ID, p.RelativeSpeed)
		}
	})
}

func TestValidateRejectsBadArch(t *testing.T) {
	good := func() ModelSpec {
		return ModelSpec{
			ID: "x", Name: "x", Kind: KindLocal, License: "mit", ParamsB: 10, ChatTemplate: "chatml",
			Source: Source{Publisher: "p", URL: "u"},
			Quants: []Quant{{Name: "q", Format: FormatGGUF, SizeBytes: 1}},
			Arch:   &Arch{AttentionLayers: 4, KVHeads: 2, HeadDim: 64},
		}
	}
	cases := map[string]func(*ModelSpec){
		"zero layers":         func(m *ModelSpec) { m.Arch.AttentionLayers = 0 },
		"zero heads":          func(m *ModelSpec) { m.Arch.KVHeads = 0 },
		"experts without k":   func(m *ModelSpec) { m.Arch.Experts = 8 },
		"k above experts":     func(m *ModelSpec) { m.Arch.Experts, m.Arch.ExpertsPerToken, m.Arch.ExpertParamsB = 2, 4, 5 },
		"experts above model": func(m *ModelSpec) { m.Arch.Experts, m.Arch.ExpertsPerToken, m.Arch.ExpertParamsB = 8, 2, 10 },
	}
	if err := (Catalog{Models: []ModelSpec{good()}}).Validate(); err != nil {
		t.Fatalf("the base fixture should validate: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := good()
			mutate(&m)
			if err := (Catalog{Models: []ModelSpec{m}}).Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
