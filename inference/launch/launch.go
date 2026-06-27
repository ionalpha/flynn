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

// Config is everything needed to build a serve command for one local model.
type Config struct {
	// BinPath is the gated runtime server executable to run (an installed build).
	BinPath string
	// WeightsPath is the verified GGUF weights file on disk to load.
	WeightsPath string
	// Model is the catalog entry being served; its ChatTemplate is the trusted contract.
	Model catalog.ModelSpec
	// Port is the loopback TCP port the server listens on. The caller picks a free one.
	Port int
	// CtxSize is the context window to run with; 0 lets the runtime use the model default.
	CtxSize int
	// CPUOnly forces the model to run entirely on the CPU with no GPU offload, the last-resort
	// footprint for a model that will not fit in device memory. The default offloads to the GPU
	// as the runtime sees fit.
	CPUOnly bool
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

// BuildPlan composes the serve command for cfg, or refuses it. It enforces that the
// weights and binary are named, the port is a usable TCP port, and the model carries a
// trusted, recognized chat template, so a plan can only ever describe a loopback-bound
// server running a known prompt contract. It performs no I/O and starts nothing.
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
	if cfg.CPUOnly {
		// Keep every layer on the CPU, so a model whose weights do not fit in device memory
		// can still serve rather than failing to start.
		argv = append(argv, "--n-gpu-layers", "0")
	}
	if cfg.APIKey != "" {
		argv = append(argv, "--api-key", cfg.APIKey)
	}

	return Plan{
		Argv:               argv,
		Host:               loopbackHost,
		Port:               cfg.Port,
		BaseURL:            fmt.Sprintf("http://%s:%d/v1", loopbackHost, cfg.Port),
		TemplateOverridden: cfg.ModelEmbedsTemplate,
	}, nil
}
