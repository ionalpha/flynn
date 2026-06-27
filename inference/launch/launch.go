// Package launch builds the exact command that serves a local model, without running
// it. It is the boundary between deciding how a model should run and actually running
// it: this package composes the gated runtime binary, the verified weights, a trusted
// prompt contract, and a loopback-only listener into a command line and the address the
// model will answer on. A caller executes that command inside the isolation sandbox and
// then reaches the model at the plan's base URL.
//
// The plan is pure data and the builder does no I/O, so what will be executed is fully
// determined and testable before anything runs. Two safety properties are enforced here
// rather than left to the runtime: the server is pinned to a loopback address so the
// model is never exposed off the machine, and the chat template is forced to a trusted,
// recognized name so a template embedded in hostile weights cannot set the prompt
// contract. A plan is refused unless both hold.
package launch

import (
	"fmt"
	"strconv"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/fault"
)

// loopbackHost is the only address a local model server is bound to. Binding anywhere
// else would expose a model, and the runtime parsing its weights, to the network.
const loopbackHost = "127.0.0.1"

// knownChatTemplates is the set of trusted chat-template names a plan may pass to the
// runtime. Restricting to recognized names matters for safety: an unrecognized name
// could make the runtime silently fall back to the template embedded in the weights,
// which is exactly the model-controlled contract the override exists to prevent. A new
// model family is supported by adding its template here once it is known good.
var knownChatTemplates = map[string]bool{
	"chatml":     true,
	"llama2":     true,
	"llama3":     true,
	"mistral-v7": true,
	"gemma":      true,
	"phi3":       true,
	"deepseek3":  true,
	"zephyr":     true,
}

// KnownChatTemplate reports whether name is a trusted template a plan may use.
func KnownChatTemplate(name string) bool { return knownChatTemplates[name] }

// Engine is the inference runtime a serve command is built for. A weight format
// determines its engine: a different format is a different command vocabulary, so the
// plan builder dispatches on it rather than assuming one runtime.
type Engine string

const (
	// EngineLlamaCpp serves GGUF weights with the llama.cpp server (CPU-offload capable,
	// the portable path). It is also the default for an unspecified format.
	EngineLlamaCpp Engine = "llama.cpp"
	// EngineVLLM serves safetensors weights with vLLM (the GPU-resident, high-throughput
	// path). Its command builder is not wired yet, so a plan for it is refused rather than
	// served with the wrong runtime.
	EngineVLLM Engine = "vllm"
)

// EngineForFormat reports which runtime serves a weight format, and whether the format
// is one a plan can be built for. An empty format defaults to llama.cpp so a caller that
// does not record a format still gets the GGUF path it had before. A pickle (code-on-load)
// format has no serving engine: it is refused at fetch and never reaches here.
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

// knownKVCacheTypes is the set of KV-cache element types a plan may request. Quantizing
// the KV cache stretches the context that fits in a fixed VRAM budget; restricting to
// recognized types keeps an unvalidated string from reaching the runtime's flag.
var knownKVCacheTypes = map[string]bool{
	"f16": true, "q8_0": true, "q4_0": true, "q4_1": true, "q5_0": true, "q5_1": true,
}

// Config is everything needed to build a serve command for one local model.
type Config struct {
	// BinPath is the gated runtime server executable to run (an installed build).
	BinPath string
	// WeightsPath is the verified GGUF weights file on disk to load.
	WeightsPath string
	// Model is the catalog entry being served; its ChatTemplate is the trusted contract.
	Model catalog.ModelSpec
	// Format is the weight encoding being served, which selects the runtime engine. Empty
	// is treated as GGUF/llama.cpp so existing callers keep their behavior.
	Format catalog.Format
	// Port is the loopback TCP port the server listens on. The caller picks a free one.
	Port int
	// CtxSize is the context window to run with; 0 lets the runtime use the model default.
	CtxSize int
	// CPUOnly forces the model to run entirely on the CPU with no GPU offload, the last-resort
	// footprint for a model that will not fit in device memory. The default offloads to the GPU
	// as the runtime sees fit. It overrides GPULayers and MoECPULayers.
	CPUOnly bool
	// GPULayers, when positive, is the number of model layers offloaded to the GPU
	// (llama.cpp -ngl). Zero leaves the offload to the runtime's default. Ignored when
	// CPUOnly is set.
	GPULayers int
	// MoECPULayers, when positive, keeps that many mixture-of-experts layers on the CPU
	// while the rest of the model stays on the GPU (llama.cpp -ncmoe). This is what lets a
	// large-total, small-active MoE run on a modest GPU: hot weights in VRAM, the cold
	// expert pages in system RAM. Ignored when CPUOnly is set.
	MoECPULayers int
	// KVCacheType, when set, quantizes the KV cache to this element type (llama.cpp
	// --cache-type-k/--cache-type-v), trading a little quality for room to hold more
	// context in the same VRAM. It must be a recognized type or the plan is refused.
	KVCacheType string
	// DraftWeightsPath, when set, names a small draft model the runtime uses for
	// speculative decoding: the draft proposes tokens the primary verifies in a batch,
	// raising throughput at no quality cost. Empty disables speculation.
	DraftWeightsPath string
	// DraftMax and DraftMin bound how many tokens the draft proposes per step (llama.cpp
	// --draft-max/--draft-min). They apply only when DraftWeightsPath is set; zero leaves
	// the runtime default.
	DraftMax int
	DraftMin int
	// APIKey, when set, is required by the server on every request, so even a local
	// process cannot reach the model without the token the caller holds. Optional.
	APIKey string
	// ModelEmbedsTemplate records that the weights carry their own chat template, a
	// signal from inspecting the file. The plan overrides it regardless; the field only
	// surfaces that an override was necessary. Optional.
	ModelEmbedsTemplate bool
}

// Plan is a built, not-yet-run serve command and where it will answer.
type Plan struct {
	// Argv is the full command: the binary followed by its flags, ready to execute.
	Argv []string
	// Host and Port are the loopback address the server binds.
	Host string
	Port int
	// BaseURL is the OpenAI-compatible endpoint a client targets once the server is up.
	BaseURL string
	// TemplateOverridden is true when the model shipped its own chat template that this
	// plan replaced with the trusted one.
	TemplateOverridden bool
}

// BuildPlan composes the serve command for cfg, or refuses it. It enforces the safety
// invariants every runtime shares, once and centrally, before handing off to the
// engine-specific command builder: the weights and binary are named, the port is a
// usable TCP port, the model carries a trusted recognized chat template, and the format
// maps to a runtime whose builder exists. So a plan can only ever describe a
// loopback-bound server running a known prompt contract, whichever engine serves it. It
// performs no I/O and starts nothing.
func BuildPlan(cfg Config) (Plan, error) {
	if cfg.BinPath == "" {
		return Plan{}, fault.New(fault.Terminal, "launch_no_binary", "launch: no runtime binary path")
	}
	if cfg.WeightsPath == "" {
		return Plan{}, fault.New(fault.Terminal, "launch_no_weights", "launch: no weights path")
	}
	if !cfg.Model.Local() {
		return Plan{}, fault.New(fault.Terminal, "launch_not_local", "launch: "+cfg.Model.ID+" is not a local model")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Plan{}, fault.New(fault.Terminal, "launch_bad_port", fmt.Sprintf("launch: port %d is out of range", cfg.Port))
	}
	tmpl := cfg.Model.ChatTemplate
	if tmpl == "" {
		return Plan{}, fault.New(fault.Terminal, "launch_no_template", "launch: "+cfg.Model.ID+" has no trusted chat template; refusing to serve without a prompt contract")
	}
	if !KnownChatTemplate(tmpl) {
		return Plan{}, fault.New(fault.Terminal, "launch_unknown_template", "launch: "+cfg.Model.ID+" chat template "+tmpl+" is not a recognized trusted template")
	}
	if cfg.KVCacheType != "" && !knownKVCacheTypes[cfg.KVCacheType] {
		return Plan{}, fault.New(fault.Terminal, "launch_bad_kv_cache", "launch: "+cfg.KVCacheType+" is not a recognized KV-cache type")
	}
	if cfg.DraftMax < 0 || cfg.DraftMin < 0 || (cfg.DraftMax > 0 && cfg.DraftMin > cfg.DraftMax) {
		return Plan{}, fault.New(fault.Terminal, "launch_bad_draft", "launch: draft token bounds are out of range")
	}

	engine, ok := EngineForFormat(cfg.Format)
	if !ok {
		return Plan{}, fault.New(fault.Terminal, "launch_bad_format", "launch: "+cfg.Model.ID+" weight format "+string(cfg.Format)+" has no serving runtime")
	}

	var argv []string
	switch engine {
	case EngineLlamaCpp:
		argv = buildLlamaCppArgv(cfg, tmpl)
	default:
		// The format selects a runtime whose command builder is not wired yet (the GPU
		// vLLM path). Refuse rather than serve it on the wrong engine.
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

// buildLlamaCppArgv builds the llama.cpp server command for cfg with the trusted
// template tmpl. The common invariants are already enforced by BuildPlan; this only
// translates the configuration into llama.cpp's flag vocabulary, including the
// efficiency levers (GPU/expert offload, KV-cache quantization, speculative decoding)
// that let a large model run well on a modest GPU.
func buildLlamaCppArgv(cfg Config, tmpl string) []string {
	argv := []string{
		cfg.BinPath,
		"--model", cfg.WeightsPath,
		"--host", loopbackHost,
		"--port", strconv.Itoa(cfg.Port),
		// Force the trusted contract; the model's embedded template is never used.
		"--chat-template", tmpl,
		// No browser UI: a headless agent server needs none, and it is attack surface.
		"--no-webui",
	}
	if cfg.CtxSize > 0 {
		argv = append(argv, "--ctx-size", strconv.Itoa(cfg.CtxSize))
	}
	switch {
	case cfg.CPUOnly:
		// Keep every layer on the CPU, so a model whose weights do not fit in device memory
		// can still serve rather than failing to start.
		argv = append(argv, "--n-gpu-layers", "0")
	default:
		if cfg.GPULayers > 0 {
			argv = append(argv, "--n-gpu-layers", strconv.Itoa(cfg.GPULayers))
		}
		if cfg.MoECPULayers > 0 {
			// Push the cold expert layers to system RAM while the rest stays on the GPU.
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
	return argv
}
