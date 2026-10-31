package localmodel_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

// entryAt returns a copy of the fixture's model under another id, at trust tier tier and
// naming origin as where it came from.
func entryAt(f *fixture, id string, tier localmodel.Trust, origin localmodel.Source) localmodel.Model {
	m := f.model
	m.Quants = slices.Clone(m.Quants)
	m.ID, m.Trust, m.Source = id, tier, origin
	return m
}

// TestNonBlessedEntryIsNeverACatalogSource is the guarantee behind [localmodel.Config.Catalog]:
// whatever an entry is called and whatever origin it records, a tier short of blessed is
// classified by that origin and never as the pinned catalog source Serve runs.
func TestNonBlessedEntryIsNeverACatalogSource(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	repo := "https://huggingface.co/acme/tiny/resolve/main/tiny.gguf"
	cases := []struct {
		name   string
		id     string
		tier   localmodel.Trust
		origin localmodel.Source
		quant  string
	}{
		{"local file", "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "local", Registry: "local", URL: "file:///tmp/x.gguf"}, repo},
		{"hub repository", "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://huggingface.co/acme/tiny"}, repo},
		{"hub claim fetched from elsewhere", "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://huggingface.co/acme/tiny"}, "https://example.test/x.gguf"},
		{"direct URL", "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"}, "https://example.test/x.gguf"},
		{"unparseable origin", "mine-1b.gguf", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "hf:broken"}, "https://example.test/x.gguf"},
		{"discovered tier", "mine:1b", localmodel.TrustDiscovered, localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"}, "https://example.test/x.gguf"},
		{"no tier", "mine:1b", "", localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"}, "https://example.test/x.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := entryAt(f, tc.id, tc.tier, tc.origin)
			m.Quants[0].URL = tc.quant
			cfg := f.config()
			cfg.Catalog = []localmodel.Model{m}
			h, err := localmodel.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = h.Close() }()

			a, err := h.Classify(m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Catalog || a.Trust == "trusted" {
				t.Fatalf("a %q-tier entry was classified as a catalog source: %+v", tc.tier, a)
			}
			if _, err := h.Serve(t.Context(), m.ID, localmodel.ServeOptions{}); !errors.Is(err, localmodel.ErrNotServable) && !errors.Is(err, localmodel.ErrNotAdmitted) {
				t.Fatalf("Serve = %v; want ErrNotServable or ErrNotAdmitted", err)
			}
			if f.hits.Load() != 0 {
				t.Fatal("a non-blessed entry caused downloads")
			}
		})
	}
}

// A directory model names its downloads as files rather than one URL. Where those files
// come from decides whether the Hub repository an entry claims is believed, and an entry
// with no download at all has nothing to vouch for the claim.
func TestNonBlessedEntryReadsMultiFileDownloads(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	repo := "https://huggingface.co/acme/tiny/resolve/main/"
	claim := localmodel.Source{Publisher: "acme", URL: "https://huggingface.co/acme/tiny"}
	cases := []struct {
		name  string
		quant localmodel.Quant
	}{
		{"every file from the repository", localmodel.Quant{Name: "bf16", Format: "safetensors", Files: []localmodel.QuantFile{
			{Name: "model.safetensors", URL: repo + "model.safetensors"}, {Name: "tokenizer.json", URL: repo + "tokenizer.json"},
		}}},
		{"one file from elsewhere", localmodel.Quant{Name: "bf16", Format: "safetensors", Files: []localmodel.QuantFile{
			{Name: "model.safetensors", URL: repo + "model.safetensors"}, {Name: "tokenizer.json", URL: "https://example.test/tokenizer.json"},
		}}},
		{"only files from elsewhere", localmodel.Quant{Name: "bf16", Format: "safetensors", Files: []localmodel.QuantFile{
			{Name: "model.safetensors", URL: "https://example.test/model.safetensors"},
		}}},
		{"no download at all", localmodel.Quant{Name: "bf16", Format: "safetensors"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := entryAt(f, "mine:1b", localmodel.TrustLocal, claim)
			m.Quants = []localmodel.Quant{tc.quant}
			cfg := f.config()
			cfg.Catalog = []localmodel.Model{m}
			h, err := localmodel.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = h.Close() }()
			a, err := h.Classify(m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Catalog {
				t.Fatalf("classified as a catalog source: %+v", a)
			}
			if a.Trust == "trusted" {
				t.Fatalf("classified as trusted: %+v", a)
			}
		})
	}
}

// A blessed entry beside a non-blessed one keeps the catalog class, so the guard narrows
// the trusted set instead of emptying it.
func TestBlessedEntryKeepsCatalogClassBesideUnverified(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mine := entryAt(f, "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"})
	cfg := f.config()
	cfg.Catalog = []localmodel.Model{f.model, mine}
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if a, err := h.Classify(f.model.ID); err != nil || !a.Catalog || a.Trust != "trusted" {
		t.Fatalf("blessed entry = %+v, %v", a, err)
	}
	if a, err := h.Classify(mine.ID); err != nil || a.Catalog {
		t.Fatalf("local entry = %+v, %v", a, err)
	}
}

// An unverified entry's publisher is a claim. If it counted as vouched, the entry would
// lift the next Hub model from that name from untrusted to semi-trusted.
func TestUnverifiedPublisherDoesNotVouchForHubModels(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mine := entryAt(f, "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"})
	cfg := f.config()
	cfg.Catalog = []localmodel.Model{mine}
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	a, err := h.Classify("hf:acme/other/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if a.Trust != "untrusted" {
		t.Fatalf("hub model from an unverified entry's publisher = %q; want untrusted", a.Trust)
	}

	cfg.Catalog = []localmodel.Model{entryAt(f, "vetted:1b", localmodel.TrustBlessed, localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"})}
	h2, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	if a, err := h2.Classify("hf:acme/other/model.gguf"); err != nil || a.Trust != "semi-trusted" {
		t.Fatalf("hub model from a blessed entry's publisher = %+v, %v; want semi-trusted", a, err)
	}
}

const gb = 1_000_000_000

// fitModel is a 4 GB model whose cache grows with the window: 128 KiB per token at f16.
func fitModel(id string, sizeBytes int64) localmodel.Model {
	return localmodel.Model{
		ID: id, Name: id, License: "mit", Kind: "local", Trust: localmodel.TrustBlessed,
		ChatTemplate: "chatml", ContextTokens: 131072, ParamsB: float64(sizeBytes) / gb,
		Source: localmodel.Source{Publisher: "tester", URL: "https://example.test/" + id},
		Quants: []localmodel.Quant{{Name: "Q4_K_M", Format: "gguf", SizeBytes: sizeBytes, URL: "https://example.test/" + id + ".gguf"}},
		Arch:   &localmodel.Arch{AttentionLayers: 32, KVHeads: 8, HeadDim: 128},
	}
}

func openFit(t *testing.T, box localmodel.Hardware, free int64, ok bool, models ...localmodel.Model) *localmodel.Host {
	t.Helper()
	f := newFixture(t, fixtureOpts{})
	cfg := f.config()
	cfg.Catalog = models
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	h.SetMachine(box, free, ok)
	return h
}

// The host judges fit the way `flynn models --fit` does: the cache is counted at the chosen
// window and the VRAM already in use is taken off. Judged by weights against total VRAM, as
// the old flat check did, this model would be feasible at every window.
func TestPlanFitCountsCacheAndFreeVRAM(t *testing.T) {
	box := localmodel.Hardware{GPUName: "test gpu", VRAMBytes: 8 * gb, RAMBytes: 4 * gb}
	h := openFit(t, box, 6*gb, true, fitModel("m:4b", 4*gb))

	short, err := h.PlanFit(t.Context(), "m:4b", localmodel.FitSettings{ContextTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if short.Fit != localmodel.FitFeasible || short.Placement != localmodel.PlacementGPU {
		t.Fatalf("8k window = %+v; want feasible on the GPU", short)
	}
	long, err := h.PlanFit(t.Context(), "m:4b", localmodel.FitSettings{ContextTokens: 131072})
	if err != nil {
		t.Fatal(err)
	}
	if long.Fit != localmodel.FitOverBudget {
		t.Fatalf("128k window = %+v; want over budget", long)
	}
	if long.KVBytes <= short.KVBytes {
		t.Fatalf("cache did not grow with the window: %d then %d", short.KVBytes, long.KVBytes)
	}
	q8, _ := h.PlanFit(t.Context(), "m:4b", localmodel.FitSettings{ContextTokens: 8192, KV: localmodel.KVQ8})
	if q8.KVBytes >= short.KVBytes {
		t.Fatalf("a q8 cache is no smaller: %d against %d", q8.KVBytes, short.KVBytes)
	}

	// Fit answers at the default window, which is wider than the 8k that fits.
	fit, err := h.Fit(t.Context(), "m:4b")
	if err != nil || fit != localmodel.FitOverBudget {
		t.Fatalf("Fit = %v, %v; want over budget at the default window", fit, err)
	}
	if _, err := h.PlanFit(t.Context(), "nope", localmodel.FitSettings{}); err == nil {
		t.Fatal("an unknown model was planned")
	}
	if fit, err := h.Fit(t.Context(), "nope"); err == nil || fit != localmodel.FitUnknown {
		t.Fatalf("Fit of an unknown model = %v, %v", fit, err)
	}
}

// Machine is what the fit view's detection resolves to.
func TestMachineMatchesFitViewDetection(t *testing.T) {
	cases := []struct {
		name string
		box  localmodel.Hardware
		free int64
		ok   bool
		want localmodel.Machine
	}{
		{"free VRAM below the total", localmodel.Hardware{VRAMBytes: 8 * gb, RAMBytes: 32 * gb}, 6 * gb, true, localmodel.Machine{VRAMFreeBytes: 6 * gb, RAMBytes: 24 * gb}},
		{"probe cannot say", localmodel.Hardware{VRAMBytes: 8 * gb, RAMBytes: 32 * gb}, 0, false, localmodel.Machine{VRAMFreeBytes: 8 * gb, RAMBytes: 24 * gb}},
		{"probe reports more than the total", localmodel.Hardware{VRAMBytes: 8 * gb, RAMBytes: 32 * gb}, 9 * gb, true, localmodel.Machine{VRAMFreeBytes: 8 * gb, RAMBytes: 24 * gb}},
		{"no GPU", localmodel.Hardware{RAMBytes: 8 * gb}, 6 * gb, true, localmodel.Machine{RAMBytes: 6 * gb}},
		{"nothing detected", localmodel.Hardware{}, 0, false, localmodel.Machine{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := openFit(t, tc.box, tc.free, tc.ok, fitModel("m:4b", 4*gb))
			if got := h.Machine(t.Context()); got != tc.want {
				t.Fatalf("Machine = %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestCPUInferenceBudget(t *testing.T) {
	for _, tc := range []struct{ ram, want int64 }{
		{0, 0},
		{1_500_000_000, 0},
		{2 * gb, 0},
		{8 * gb, 6 * gb},
		{100 * gb, 75 * gb},
	} {
		if got := localmodel.CPUInferenceBudget(tc.ram); got != tc.want {
			t.Errorf("CPUInferenceBudget(%d) = %d; want %d", tc.ram, got, tc.want)
		}
	}
}

// Rank puts the models that run before the ones that do not, and honours the query.
func TestRankOrdersByFitForThisMachine(t *testing.T) {
	box := localmodel.Hardware{GPUName: "test gpu", VRAMBytes: 8 * gb, RAMBytes: 4 * gb}
	big, small := fitModel("big:30b", 30*gb), fitModel("small:4b", 4*gb)
	h := openFit(t, box, 6*gb, true, big, small)

	ranked := h.Rank(t.Context(), localmodel.ModelQuery{}, localmodel.FitSettings{ContextTokens: 8192})
	if len(ranked) != 2 || ranked[0].Model.ID != "small:4b" || ranked[1].Model.ID != "big:30b" {
		t.Fatalf("ranked = %+v; want small first", ranked)
	}
	if !ranked[0].Plan.Fit.Runnable() || ranked[1].Plan.Fit.Runnable() {
		t.Fatalf("verdicts = %v, %v", ranked[0].Plan.Fit, ranked[1].Plan.Fit)
	}
	only := h.Rank(t.Context(), localmodel.ModelQuery{MaxParamsB: 5}, localmodel.FitSettings{})
	if len(only) != 1 || only[0].Model.ID != "small:4b" {
		t.Fatalf("ranked under the query = %+v", only)
	}
}

// Hardware reports the box the host detected, the same one Machine plans against.
func TestHardwareReportsTheDetectedBox(t *testing.T) {
	box := localmodel.Hardware{GPUName: "Test GPU", VRAMBytes: 8 * gb, RAMBytes: 16 * gb}
	h := openFit(t, box, 6*gb, true)
	if got := h.Hardware(t.Context()); got != box {
		t.Fatalf("Hardware = %+v; want %+v", got, box)
	}
}
