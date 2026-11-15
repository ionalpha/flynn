// Package launch builds, without running it, the command that serves a local model: the
// gated runtime binary, the verified weights, a trusted prompt contract and a
// loopback-only listener, plus the address the model will answer on. A caller runs the
// command inside the sandbox. The builder does no I/O, so the plan is testable before
// anything runs. A plan is refused unless the server binds loopback, so the model is never
// exposed off the machine, and the chat template is a trusted name, so hostile weights
// cannot set the prompt contract.
package launch

import (
	"fmt"
	"strconv"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/catalog"
)

// loopbackHost is the only bind address, because any other exposes the model, and the
// runtime parsing its weights, to the network.
const loopbackHost = "127.0.0.1"

// knownChatTemplates is the set of trusted template names a plan may pass. An
// unrecognized name could make the runtime fall back to the template in the weights,
// the model-controlled contract the override exists to prevent. A name llama.cpp does
// not ship also needs its reviewed source embedded; see templateArgs.
var knownChatTemplates = map[string]bool{
	"chatml":     true,
	"llama2":     true,
	"llama3":     true,
	"mistral-v7": true,
	"gemma":      true,
	"phi3":       true,
	"deepseek3":  true,
	"zephyr":     true,
	"gpt-oss":    true,
	"gemma4":     true,
}

// KnownChatTemplate reports whether name is a trusted template a plan may use.
func KnownChatTemplate(name string) bool { return knownChatTemplates[name] }

// Engine is the runtime a serve command is built for. Each weight format has its own
// engine and command vocabulary, so the plan builder dispatches on it.
type Engine string

const (
	// EngineLlamaCpp serves GGUF weights with the llama.cpp server, the portable path
	// with CPU offload. It is the default for an unspecified format.
	EngineLlamaCpp Engine = "llama.cpp"
	// EngineVLLM serves safetensors weights with vLLM, the GPU-resident path.
	EngineVLLM Engine = "vllm"
)

// EngineForFormat reports which runtime serves a weight format, and whether a plan can
// be built for it. An empty format means llama.cpp. A pickle format has no engine; it is
// refused at fetch, because loading it runs code.
func EngineForFormat(f catalog.Format) (Engine, bool) {
	switch f {
	case "", catalog.FormatGGUF:
		return EngineLlamaCpp, true
	case catalog.FormatSafetensors:
		return EngineVLLM, true
	default:
		return "", false
	}
}

// knownKVCacheTypes is the set of KV-cache element types a plan may request, so an
// unvalidated string never reaches the runtime's flag.
var knownKVCacheTypes = map[string]bool{
	"f16": true, "q8_0": true, "q4_0": true, "q4_1": true, "q5_0": true, "q5_1": true,
}

// knownVLLMQuant is the set of --quantization schemes a vLLM plan may pass. An
// unrecognized scheme could make the runtime guess or fall back, so only schemes that map
// to a real vLLM kernel are allowed. NVFP4 checkpoints use "modelopt_fp4".
var knownVLLMQuant = map[string]bool{
	"awq": true, "awq_marlin": true, "gptq": true, "gptq_marlin": true, "marlin": true,
	"fp8": true, "modelopt": true, "modelopt_fp4": true, "compressed-tensors": true,
	"bitsandbytes": true,
}

// KnownVLLMQuant reports whether scheme is a vLLM quantization a plan may request.
func KnownVLLMQuant(scheme string) bool { return knownVLLMQuant[scheme] }

// knownVLLMKVCacheDtype is the set of --kv-cache-dtype values a vLLM plan may request.
// vLLM names these differently from llama.cpp, so it has its own set.
var knownVLLMKVCacheDtype = map[string]bool{
	"auto": true, "fp8": true, "fp8_e4m3": true, "fp8_e5m2": true,
}

// Config is everything needed to build a serve command for one local model.
type Config struct {
	// BinPath is the gated runtime server executable.
	BinPath string
	// WeightsPath is the verified weights file.
	WeightsPath string
	// Model is the catalog entry; its ChatTemplate is the trusted contract.
	Model catalog.ModelSpec
	// Format selects the runtime engine. Empty means GGUF on llama.cpp.
	Format catalog.Format
	// Port is the loopback TCP port. The caller picks a free one.
	Port int
	// CtxSize is the context window; 0 uses the model default.
	CtxSize int
	// CPUOnly keeps the whole model on the CPU, the last-resort footprint for a model
	// that does not fit in device memory. It overrides GPULayers and MoECPULayers.
	CPUOnly bool
	// GPULayers, when positive, is the number of layers offloaded to the GPU (llama.cpp
	// -ngl).
	GPULayers int
	// MoECPULayers, when positive, keeps that many mixture-of-experts layers on the CPU
	// (llama.cpp -ncmoe), so a large MoE with few active experts runs on a modest GPU.
	MoECPULayers int
	// KVCacheType quantizes the llama.cpp KV cache (--cache-type-k/-v) to fit more
	// context in the same VRAM. An unrecognized type is refused.
	KVCacheType string
	// DraftWeightsPath names a small draft model for speculative decoding: it proposes
	// tokens the primary verifies in a batch, raising throughput at no quality cost.
	DraftWeightsPath string
	// DraftMax and DraftMin bound the tokens drafted per step (llama.cpp
	// --draft-max/--draft-min); zero uses the runtime default.
	DraftMax int
	DraftMin int
	// Quantization is the vLLM --quantization scheme, for example "awq", "fp8" or
	// "modelopt_fp4" for NVFP4. An unrecognized scheme is refused. llama.cpp reads the
	// quantization from the GGUF file.
	Quantization string
	// GPUMemoryUtilization is the fraction of GPU memory vLLM may reserve, in (0,1]
	// (--gpu-memory-utilization). Zero uses vLLM's default.
	GPUMemoryUtilization float64
	// KVCacheDtype is the vLLM counterpart to KVCacheType (--kv-cache-dtype), for
	// example "fp8". An unrecognized type is refused.
	KVCacheDtype string
	// Containerized makes vLLM bind all interfaces inside the container's network
	// namespace, so the engine's published port can reach it. The engine publishes that
	// port on host loopback only, so the server is still unreachable off the host.
	Containerized bool
	// APIKey, when set, is required on every request, so another local process cannot
	// reach the model without the caller's token.
	APIKey string
	// APIKeyFile names a file holding the key (llama.cpp --api-key-file), so the token
	// stays out of argv, which any local user can read. It is refused for vLLM, which has
	// no such flag.
	APIKeyFile string
	// ModelEmbedsTemplate records that the weights carry their own chat template. The
	// plan overrides it either way.
	ModelEmbedsTemplate bool
}

// Plan is a built, not-yet-run serve command and where it will answer.
type Plan struct {
	// Argv is the binary followed by its flags.
	Argv []string
	// Host and Port are the loopback address the server binds.
	Host string
	Port int
	// BaseURL is the OpenAI-compatible endpoint a client targets.
	BaseURL string
	// TemplateOverridden is true when the plan replaced a template the model shipped.
	TemplateOverridden bool
}

// BuildPlan composes the serve command for cfg, or refuses it. It checks the invariants
// every engine shares before the engine-specific builder runs: binary and weights named, a
// usable port, a trusted chat template and a format with a wired engine. So any plan
// describes a loopback-bound server with a known prompt contract.
func BuildPlan(cfg Config) (Plan, error) {
	if err := validateIdentity(cfg); err != nil {
		return Plan{}, err
	}
	if err := validateTemplate(cfg); err != nil {
		return Plan{}, err
	}
	if err := validateTuning(cfg); err != nil {
		return Plan{}, err
	}

	engine, ok := EngineForFormat(cfg.Format)
	if !ok {
		return Plan{}, fault.New(fault.Terminal, "launch_bad_format", "launch: "+cfg.Model.ID+" weight format "+string(cfg.Format)+" has no serving runtime")
	}

	var argv []string
	switch engine {
	case EngineLlamaCpp:
		argv = buildLlamaCppArgv(cfg, cfg.Model.ChatTemplate)
	case EngineVLLM:
		argv = buildVLLMArgv(cfg)
	default:
		// The engine's builder is not wired, so refuse instead of serving on the wrong one.
		return Plan{}, fault.New(fault.Terminal, "launch_engine_unavailable", "launch: serving "+cfg.Model.ID+" needs the "+string(engine)+" runtime, which is not available yet")
	}

	return Plan{
		Argv:               argv,
		Host:               loopbackHost,
		Port:               cfg.Port,
		BaseURL:            fmt.Sprintf("http://%s:%d/v1", loopbackHost, cfg.Port),
		TemplateOverridden: cfg.ModelEmbedsTemplate,
	}, nil
}

func validateIdentity(cfg Config) error {
	if cfg.BinPath == "" {
		return fault.New(fault.Terminal, "launch_no_binary", "launch: no runtime binary path")
	}
	if cfg.WeightsPath == "" {
		return fault.New(fault.Terminal, "launch_no_weights", "launch: no weights path")
	}
	if !cfg.Model.Local() {
		return fault.New(fault.Terminal, "launch_not_local", "launch: "+cfg.Model.ID+" is not a local model")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fault.New(fault.Terminal, "launch_bad_port", fmt.Sprintf("launch: port %d is out of range", cfg.Port))
	}
	return nil
}

// validateTemplate refuses a model with no trusted chat template.
func validateTemplate(cfg Config) error {
	tmpl := cfg.Model.ChatTemplate
	if tmpl == "" {
		return fault.New(fault.Terminal, "launch_no_template", "launch: "+cfg.Model.ID+" has no trusted chat template; refusing to serve without a prompt contract")
	}
	if !KnownChatTemplate(tmpl) {
		return fault.New(fault.Terminal, "launch_unknown_template", "launch: "+cfg.Model.ID+" chat template "+tmpl+" is not a recognized trusted template")
	}
	return nil
}

// validateTuning refuses an efficiency knob outside its range. It runs after the identity
// and template checks so a structural fault is reported first, and before the engine is
// chosen so a bad knob is refused on either engine.
func validateTuning(cfg Config) error {
	if cfg.KVCacheType != "" && !knownKVCacheTypes[cfg.KVCacheType] {
		return fault.New(fault.Terminal, "launch_bad_kv_cache", "launch: "+cfg.KVCacheType+" is not a recognized KV-cache type")
	}
	if cfg.DraftMax < 0 || cfg.DraftMin < 0 || (cfg.DraftMax > 0 && cfg.DraftMin > cfg.DraftMax) {
		return fault.New(fault.Terminal, "launch_bad_draft", "launch: draft token bounds are out of range")
	}
	if cfg.Quantization != "" && !knownVLLMQuant[cfg.Quantization] {
		return fault.New(fault.Terminal, "launch_bad_quant", "launch: "+cfg.Quantization+" is not a recognized vLLM quantization scheme")
	}
	if cfg.KVCacheDtype != "" && !knownVLLMKVCacheDtype[cfg.KVCacheDtype] {
		return fault.New(fault.Terminal, "launch_bad_kv_dtype", "launch: "+cfg.KVCacheDtype+" is not a recognized vLLM KV-cache dtype")
	}
	if cfg.GPUMemoryUtilization < 0 || cfg.GPUMemoryUtilization > 1 {
		return fault.New(fault.Terminal, "launch_bad_gpu_util", "launch: GPU memory utilization must be in (0,1]")
	}
	if cfg.APIKeyFile != "" && cfg.APIKey != "" {
		return fault.New(fault.Terminal, "launch_key_conflict", "launch: set either APIKey or APIKeyFile, not both")
	}
	if cfg.APIKeyFile != "" {
		if engine, ok := EngineForFormat(cfg.Format); ok && engine == EngineVLLM {
			return fault.New(fault.Terminal, "launch_key_file_unsupported", "launch: the vLLM runtime has no key-file flag; refusing to put the key on the command line")
		}
	}
	return nil
}

// buildLlamaCppArgv translates cfg into llama.cpp flags with the trusted template tmpl.
// BuildPlan has already enforced the shared invariants.
func buildLlamaCppArgv(cfg Config, tmpl string) []string {
	argv := []string{
		cfg.BinPath,
		"--model", cfg.WeightsPath,
		"--host", loopbackHost,
		"--port", strconv.Itoa(cfg.Port),
		// No web UI: an agent server needs none, and it is attack surface.
		"--no-webui",
	}
	// The model's embedded template is never used.
	argv = append(argv, templateArgs(tmpl)...)
	if cfg.CtxSize > 0 {
		argv = append(argv, "--ctx-size", strconv.Itoa(cfg.CtxSize))
	}
	switch {
	case cfg.CPUOnly:
		// A model that does not fit in device memory still serves on the CPU.
		argv = append(argv, "--n-gpu-layers", "0")
	default:
		if cfg.GPULayers > 0 {
			argv = append(argv, "--n-gpu-layers", strconv.Itoa(cfg.GPULayers))
		}
		if cfg.MoECPULayers > 0 {
			// Cold expert layers go to system RAM; the rest stays on the GPU.
			argv = append(argv, "--n-cpu-moe", strconv.Itoa(cfg.MoECPULayers))
		}
	}
	if cfg.KVCacheType != "" {
		argv = append(argv, "--cache-type-k", cfg.KVCacheType, "--cache-type-v", cfg.KVCacheType)
	}
	if cfg.DraftWeightsPath != "" {
		argv = append(argv, "--model-draft", cfg.DraftWeightsPath)
		if cfg.DraftMax > 0 {
			argv = append(argv, "--draft-max", strconv.Itoa(cfg.DraftMax))
		}
		if cfg.DraftMin > 0 {
			argv = append(argv, "--draft-min", strconv.Itoa(cfg.DraftMin))
		}
	}
	if cfg.APIKey != "" {
		argv = append(argv, "--api-key", cfg.APIKey)
	}
	if cfg.APIKeyFile != "" {
		argv = append(argv, "--api-key-file", cfg.APIKeyFile)
	}
	return argv
}

// buildVLLMArgv translates cfg into vLLM flags. BuildPlan has already enforced the shared
// invariants, so the server is the same loopback-bound, optionally key-gated endpoint as
// the llama.cpp one. The model is served under its catalog id. The first element is an
// absolute path for a host process, or the in-image command for a container.
//
// It passes no chat-template flag, because vLLM applies the tokenizer's template. The
// entry must still name a trusted template, so an entry without a vetted contract is
// refused on either engine.
func buildVLLMArgv(cfg Config) []string {
	// See Config.Containerized: a container binds all interfaces, a host process binds
	// loopback.
	bindHost := loopbackHost
	if cfg.Containerized {
		bindHost = "0.0.0.0"
	}
	argv := []string{
		cfg.BinPath, "serve", cfg.WeightsPath,
		"--host", bindHost,
		"--port", strconv.Itoa(cfg.Port),
		"--served-model-name", cfg.Model.ID,
	}
	if cfg.Quantization != "" {
		argv = append(argv, "--quantization", cfg.Quantization)
	}
	if cfg.GPUMemoryUtilization > 0 {
		argv = append(argv, "--gpu-memory-utilization", strconv.FormatFloat(cfg.GPUMemoryUtilization, 'f', -1, 64))
	}
	if cfg.CtxSize > 0 {
		// vLLM calls the context window the maximum model length.
		argv = append(argv, "--max-model-len", strconv.Itoa(cfg.CtxSize))
	}
	if cfg.KVCacheDtype != "" {
		argv = append(argv, "--kv-cache-dtype", cfg.KVCacheDtype)
	}
	if cfg.APIKey != "" {
		argv = append(argv, "--api-key", cfg.APIKey)
	}
	return argv
}
