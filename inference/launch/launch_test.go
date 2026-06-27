package launch

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/catalog"
)

// localModel is a minimal local catalog entry for building plans in tests.
func localModel(tmpl string) catalog.ModelSpec {
	return catalog.ModelSpec{
		ID: "ollama:test:1b", Kind: catalog.KindLocal, ChatTemplate: tmpl,
		Quants: []catalog.Quant{{Name: "Q4_K_M", Format: catalog.FormatGGUF}},
	}
}

// argvHasFlag reports whether argv contains flag immediately followed by want.
func argvHasFlag(argv []string, flag, want string) bool {
	for i := range len(argv) - 1 {
		if argv[i] == flag && argv[i+1] == want {
			return true
		}
	}
	return false
}

func TestBuildPlanBuildsLoopbackServeCommand(t *testing.T) {
	cfg := Config{BinPath: "/rt/llama-server", WeightsPath: "/w/model.gguf", Model: localModel("chatml"), Port: 8123}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Host != "127.0.0.1" || plan.Port != 8123 {
		t.Fatalf("plan should bind loopback:8123, got %s:%d", plan.Host, plan.Port)
	}
	if plan.BaseURL != "http://127.0.0.1:8123/v1" {
		t.Fatalf("base url %q", plan.BaseURL)
	}
	if plan.Argv[0] != "/rt/llama-server" {
		t.Fatalf("argv must start with the binary, got %q", plan.Argv[0])
	}
	for _, want := range [][2]string{{"--model", "/w/model.gguf"}, {"--host", "127.0.0.1"}, {"--port", "8123"}, {"--chat-template", "chatml"}} {
		if !argvHasFlag(plan.Argv, want[0], want[1]) {
			t.Fatalf("argv missing %s %s: %v", want[0], want[1], plan.Argv)
		}
	}
	if !contains(plan.Argv, "--no-webui") {
		t.Fatalf("argv should disable the web ui: %v", plan.Argv)
	}
}

func TestBuildPlanOptionalFlags(t *testing.T) {
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("llama3"), Port: 9000, CtxSize: 4096, APIKey: "tok"}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasFlag(plan.Argv, "--ctx-size", "4096") {
		t.Fatalf("ctx size not passed: %v", plan.Argv)
	}
	if !argvHasFlag(plan.Argv, "--api-key", "tok") {
		t.Fatalf("api key not passed: %v", plan.Argv)
	}
}

func TestBuildPlanCPUOnlyForcesNoGPULayers(t *testing.T) {
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000, CPUOnly: true}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasFlag(plan.Argv, "--n-gpu-layers", "0") {
		t.Fatalf("a CPU-only run must force zero GPU layers: %v", plan.Argv)
	}
}

func TestBuildPlanDefaultDoesNotForceGPULayers(t *testing.T) {
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Argv {
		if a == "--n-gpu-layers" {
			t.Fatalf("a default run must leave GPU offload to the runtime: %v", plan.Argv)
		}
	}
}

func TestBuildPlanRefuses(t *testing.T) {
	base := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 8000}
	apiModel := catalog.ModelSpec{ID: "anthropic:x", Kind: catalog.KindAPI}
	cases := map[string]Config{
		"no binary":        {WeightsPath: "w", Model: localModel("chatml"), Port: 8000},
		"no weights":       {BinPath: "b", Model: localModel("chatml"), Port: 8000},
		"not local":        {BinPath: "b", WeightsPath: "w", Model: apiModel, Port: 8000},
		"port zero":        {BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 0},
		"port too high":    {BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 70000},
		"no template":      {BinPath: "b", WeightsPath: "w", Model: localModel(""), Port: 8000},
		"unknown template": {BinPath: "b", WeightsPath: "w", Model: localModel("evil-jinja"), Port: 8000},
	}
	if _, err := BuildPlan(base); err != nil {
		t.Fatalf("the base config should be valid: %v", err)
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildPlan(cfg); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

func TestBuildPlanRecordsTemplateOverride(t *testing.T) {
	plan, err := BuildPlan(Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 8000, ModelEmbedsTemplate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.TemplateOverridden {
		t.Fatal("a model that embeds a template should be recorded as overridden")
	}
}

func TestBuildPlanEfficiencyFlags(t *testing.T) {
	cfg := Config{
		BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000,
		GPULayers: 99, MoECPULayers: 24, KVCacheType: "q8_0",
		DraftWeightsPath: "/w/draft.gguf", DraftMax: 16, DraftMin: 2,
	}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][2]string{
		{"--n-gpu-layers", "99"},
		{"--n-cpu-moe", "24"},
		{"--cache-type-k", "q8_0"},
		{"--cache-type-v", "q8_0"},
		{"--model-draft", "/w/draft.gguf"},
		{"--draft-max", "16"},
		{"--draft-min", "2"},
	} {
		if !argvHasFlag(plan.Argv, want[0], want[1]) {
			t.Fatalf("argv missing %s %s: %v", want[0], want[1], plan.Argv)
		}
	}
}

func TestBuildPlanCPUOnlyOverridesOffload(t *testing.T) {
	// CPU-only must win over GPU/expert offload, forcing every layer onto the CPU.
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000, CPUOnly: true, GPULayers: 99, MoECPULayers: 24}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasFlag(plan.Argv, "--n-gpu-layers", "0") {
		t.Fatalf("CPU-only must force zero GPU layers: %v", plan.Argv)
	}
	if contains(plan.Argv, "--n-cpu-moe") {
		t.Fatalf("CPU-only must not also emit an expert-offload flag: %v", plan.Argv)
	}
}

func TestBuildPlanDraftBoundsNeedDraftModel(t *testing.T) {
	// Draft bounds without a draft model emit nothing, never a dangling flag.
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000, DraftMax: 8}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if contains(plan.Argv, "--draft-max") || contains(plan.Argv, "--model-draft") {
		t.Fatalf("draft flags must not appear without a draft model: %v", plan.Argv)
	}
}

func TestBuildPlanRefusesBadEfficiencyConfig(t *testing.T) {
	base := func() Config {
		return Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Port: 9000}
	}
	cases := map[string]func(*Config){
		"unknown kv cache type": func(c *Config) { c.KVCacheType = "q3_garbage" },
		"negative draft max":    func(c *Config) { c.DraftMax = -1 },
		"min above max":         func(c *Config) { c.DraftWeightsPath = "d"; c.DraftMax = 4; c.DraftMin = 8 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mut(&cfg)
			if _, err := BuildPlan(cfg); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

func TestEngineForFormat(t *testing.T) {
	for _, c := range []struct {
		format catalog.Format
		engine Engine
		ok     bool
	}{
		{"", EngineLlamaCpp, true},
		{catalog.FormatGGUF, EngineLlamaCpp, true},
		{catalog.FormatSafetensors, EngineVLLM, true},
		{catalog.FormatPickle, "", false},
	} {
		got, ok := EngineForFormat(c.format)
		if ok != c.ok || got != c.engine {
			t.Fatalf("EngineForFormat(%q) = %q,%v; want %q,%v", c.format, got, ok, c.engine, c.ok)
		}
	}
}

func TestBuildPlanBuildsVLLMServeCommand(t *testing.T) {
	// A safetensors model selects the vLLM engine and produces a vllm serve command bound
	// to the same loopback endpoint the llama.cpp path uses.
	cfg := Config{
		BinPath: "vllm", WeightsPath: "/w/model", Model: localModel("chatml"),
		Format: catalog.FormatSafetensors, Port: 9000, CtxSize: 8192, APIKey: "tok",
		Quantization: "modelopt_fp4", GPUMemoryUtilization: 0.9, KVCacheDtype: "fp8",
	}
	plan, err := BuildPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Host != "127.0.0.1" || plan.Port != 9000 || plan.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("vLLM plan should bind the loopback endpoint, got %s:%d %s", plan.Host, plan.Port, plan.BaseURL)
	}
	if plan.Argv[0] != "vllm" || plan.Argv[1] != "serve" || plan.Argv[2] != "/w/model" {
		t.Fatalf("argv must be `vllm serve <model>`, got %v", plan.Argv[:3])
	}
	for _, want := range [][2]string{
		{"--host", "127.0.0.1"},
		{"--port", "9000"},
		{"--served-model-name", "ollama:test:1b"},
		{"--quantization", "modelopt_fp4"},
		{"--gpu-memory-utilization", "0.9"},
		{"--max-model-len", "8192"},
		{"--kv-cache-dtype", "fp8"},
		{"--api-key", "tok"},
	} {
		if !argvHasFlag(plan.Argv, want[0], want[1]) {
			t.Fatalf("argv missing %s %s: %v", want[0], want[1], plan.Argv)
		}
	}
	// vLLM gets no llama.cpp-only flags.
	if contains(plan.Argv, "--no-webui") || contains(plan.Argv, "--model") {
		t.Fatalf("vLLM argv must not carry llama.cpp flags: %v", plan.Argv)
	}
}

func TestBuildPlanVLLMRefusesBadKnobs(t *testing.T) {
	base := func() Config {
		return Config{BinPath: "vllm", WeightsPath: "/w/model", Model: localModel("chatml"), Format: catalog.FormatSafetensors, Port: 9000}
	}
	cases := map[string]func(*Config){
		"bad quant":     func(c *Config) { c.Quantization = "made-up" },
		"bad kv dtype":  func(c *Config) { c.KVCacheDtype = "q4_0" }, // a llama.cpp type, not a vLLM one
		"util over 1":   func(c *Config) { c.GPUMemoryUtilization = 1.5 },
		"util negative": func(c *Config) { c.GPUMemoryUtilization = -0.1 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mut(&cfg)
			if _, err := BuildPlan(cfg); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

func TestBuildPlanGGUFFormatStillServes(t *testing.T) {
	// An explicit GGUF format takes the llama.cpp path exactly as the empty default does.
	cfg := Config{BinPath: "b", WeightsPath: "w", Model: localModel("chatml"), Format: catalog.FormatGGUF, Port: 9000}
	if _, err := BuildPlan(cfg); err != nil {
		t.Fatalf("an explicit GGUF format must still serve: %v", err)
	}
}

// buildMiniGGUF encodes a minimal valid GGUF header carrying the given string metadata,
// enough for the reader to extract the chat template.
func buildMiniGGUF(kvs map[string]string) []byte {
	const magic uint32 = 0x46554747
	const typeString uint32 = 8
	var b bytes.Buffer
	w32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	wstr := func(s string) {
		w64(uint64(len(s)))
		b.WriteString(s)
	}
	w32(magic)
	w32(3)
	w64(0)
	w64(uint64(len(kvs)))
	for k, v := range kvs {
		wstr(k)
		w32(typeString)
		wstr(v)
	}
	return b.Bytes()
}

func TestInspectTemplate(t *testing.T) {
	dir := t.TempDir()
	withTmpl := filepath.Join(dir, "with.gguf")
	if err := os.WriteFile(withTmpl, buildMiniGGUF(map[string]string{"tokenizer.chat_template": "{{ messages }}"}), 0o600); err != nil {
		t.Fatal(err)
	}
	dec, err := InspectTemplate(withTmpl, "chatml")
	if err != nil {
		t.Fatal(err)
	}
	if dec.Template != "chatml" {
		t.Fatalf("the decision must use the trusted template, got %q", dec.Template)
	}
	if !dec.ModelSupplied {
		t.Fatal("a model embedding a template should be flagged as model-supplied")
	}

	plain := filepath.Join(dir, "plain.gguf")
	if err := os.WriteFile(plain, buildMiniGGUF(map[string]string{"general.name": "x"}), 0o600); err != nil {
		t.Fatal(err)
	}
	dec, err = InspectTemplate(plain, "chatml")
	if err != nil {
		t.Fatal(err)
	}
	if dec.ModelSupplied {
		t.Fatal("a model with no embedded template must not be flagged")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// guard against an accidental non-loopback constant.
func TestLoopbackHostIsLoopback(t *testing.T) {
	if !strings.HasPrefix(loopbackHost, "127.") {
		t.Fatalf("the serve host must be loopback, got %q", loopbackHost)
	}
}
