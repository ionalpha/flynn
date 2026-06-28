package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference/launch"
	"github.com/ionalpha/flynn/inference/provision"
	"github.com/ionalpha/flynn/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// This file is the GPU serving path: it serves a safetensors model with vLLM inside a
// container, the counterpart to the llama.cpp process path in modelrun.go. Everything it
// needs is acquired through Flynn's own verified primitives, so a machine with nothing but a
// GPU and a container engine ends up talking to a running model with no manual step: the
// runtime image is pulled by its pinned digest, the model files are fetched and verified one
// by one, and the server runs under the sandbox's untrusted guarantees (read-only weights,
// dropped capabilities, a secret-free environment, an offline runtime, a loopback-only port)
// and is supervised on the same serve manager as a process-backed model.

// vllmMountPath is where the verified model directory is mounted read-only inside the
// container; the serve command loads the model from here, never from a host path.
const vllmMountPath = "/model"

// vllmNetwork is the named network the serving container joins, so it is on a stable named
// network rather than the engine's shared default bridge and its server is reached only over
// the host's loopback-published port.
const vllmNetwork = "flynn-serve"

// serveContainerModel serves a safetensors model with vLLM in a container and returns its
// running loopback endpoint. It refuses cleanly when the host cannot run the GPU path, when
// the quant has no file manifest, or when the pinned runtime image is gated, so it never
// half-starts. Acquisition (image pull, model fetch) and the run all go through Flynn's
// verified, sandboxed primitives.
func (r *localRunner) serveContainerModel(ctx context.Context, m catalog.ModelSpec, q catalog.Quant, ctxSize int) (serve.Endpoint, error) {
	if !r.box.Containers.GPUPassthrough() || !r.box.HasGPU() {
		return serve.Endpoint{}, fmt.Errorf(
			"%q needs the vLLM GPU runtime (a container engine, the NVIDIA toolkit, and a GPU), which this host does not provide, and it has no GGUF quant to fall back to", m.ID,
		)
	}
	if !q.MultiFile() {
		return serve.Endpoint{}, fmt.Errorf("%q vLLM quant %q has no file manifest to fetch", m.ID, q.Name)
	}

	// Acquire the runtime image: pull it by its pinned digest through the verified path,
	// gated against the advisory floor before any network access.
	rel := provision.VLLMImage()
	if err := rel.Gate(); err != nil {
		return serve.Endpoint{}, fmt.Errorf("refusing to run the vLLM image: %w", err)
	}
	prov := rel.Provisioner(func(ctx context.Context, ref, digest string) error {
		_, _ = fmt.Fprintf(r.out, "provisioning vLLM runtime image %s (verifying digest)\n", ref)
		return sandbox.PullImage(ctx, sandbox.EngineDocker, ref, digest)
	})
	acq, err := prov.Acquire(ctx)
	if err != nil {
		return serve.Endpoint{}, fmt.Errorf("provision vLLM image: %w", err)
	}

	// Fetch the model directory: every file verified, written under one model dir.
	modelDir, err := r.ensureModelDir(ctx, m, q)
	if err != nil {
		return serve.Endpoint{}, err
	}

	port, err := r.freePort()
	if err != nil {
		return serve.Endpoint{}, fmt.Errorf("pick a loopback port: %w", err)
	}

	// Build the vLLM serve command. The weights path is the in-container mount target, not a
	// host path, and Containerized makes the server bind all interfaces inside its isolated
	// namespace so the host's loopback-published port reaches it.
	plan, err := launch.BuildPlan(launch.Config{
		BinPath:       "vllm", // the image's entrypoint command
		WeightsPath:   vllmMountPath,
		Model:         m,
		Format:        q.Format,
		Port:          port,
		CtxSize:       ctxSize,
		Containerized: true,
		Quantization:  vllmQuantScheme(q),
		// A conservative default that leaves headroom for whatever else holds device memory
		// (a desktop GPU also drives the display); vLLM refuses to start if it cannot reserve
		// this fraction of total VRAM, so aiming too high fails on a shared GPU.
		GPUMemoryUtilization: 0.80,
	})
	if err != nil {
		return serve.Endpoint{}, err
	}

	// Prepare the named network the published container joins.
	if err := sandbox.EnsureContainerNetwork(ctx, sandbox.EngineDocker, vllmNetwork); err != nil {
		return serve.Endpoint{}, fmt.Errorf("prepare serving network: %w", err)
	}

	// Assemble the container request under the untrusted guarantees: the weights mounted
	// read-only, the resource caps, dropped capabilities, and a secret-free environment that
	// also points vLLM's caches at the in-memory /tmp (the root is read-only) and forces the
	// runtime offline so it makes no outbound request and serves only the mounted,
	// pre-verified weights.
	g := sandbox.Untrusted(
		sandbox.Limits{MemMiB: vllmMemMiB(q), VCPUs: 4, PIDs: 4096},
		sandbox.Mount{HostPath: modelDir, GuestPath: vllmMountPath},
	)
	g.Env = map[string]string{
		"HOME":                 "/tmp",
		"HF_HOME":              "/tmp/hf",
		"XDG_CACHE_HOME":       "/tmp/cache",
		"VLLM_CACHE_ROOT":      "/tmp/vllm",
		"OUTLINES_CACHE_DIR":   "/tmp/outlines",
		"HF_HUB_OFFLINE":       "1",
		"TRANSFORMERS_OFFLINE": "1",
		"VLLM_NO_USAGE_STATS":  "1",
		"DO_NOT_TRACK":         "1",
	}
	spec := sandbox.ContainerSpec{
		Image:         sandbox.ContainerImage{Ref: acq.Image.Ref, Digest: acq.Image.Digest},
		Guarantees:    g,
		GPU:           sandbox.GPURequest{Enabled: true},
		Network:       vllmNetwork,
		HostPort:      port,
		ContainerPort: port,
		Command:       plan.Argv,
		// vLLM JIT-compiles its CUDA kernels into the scratch dir at startup and runs them
		// from there, so the in-memory scratch must be executable.
		ExecScratch: true,
	}

	_, _ = fmt.Fprintf(r.out, "starting vLLM for %s on the GPU (loading weights, this can take a minute)\n", m.ID)
	return r.manager.EnsureContainer(ctx, serve.ContainerEnsureConfig{
		ModelID: m.ID,
		Runtime: "vllm",
		Spec:    spec,
		BaseURL: plan.BaseURL,
		Port:    port,
		// A GPU server's first start loads weights, captures CUDA graphs, and JIT-compiles
		// kernels, which on a cold cache runs well past the process-oriented default; give it
		// room rather than tearing down a server that is still coming up.
		ReadyTimeout: 6 * time.Minute,
	})
}

// ensureModelDir fetches every file of a multi-file model into a per-model directory under
// the data dir, reusing a directory that is already complete so a re-run does no network.
func (r *localRunner) ensureModelDir(ctx context.Context, m catalog.ModelSpec, q catalog.Quant) (string, error) {
	dir := filepath.Join(r.dataDir, "models", modelDirName(m.ID, q))
	files := make([]provision.ModelFile, len(q.Files))
	for i, f := range q.Files {
		files[i] = provision.ModelFile{Name: f.Name, URL: f.URL, SHA256: f.Digest, SizeBytes: f.SizeBytes}
	}
	if provision.ModelDirPresent(files, dir) {
		return dir, nil
	}
	_, _ = fmt.Fprintf(r.out, "fetching %s (%s, %d files, %s)\n", m.ID, q.Name, len(files), humanBytes(q.SizeBytes))
	return provision.FetchModelDir(ctx, fetch.New(), files, dir)
}

// modelDirName builds a filesystem-safe directory name for a model's weights, so two models
// or two quants never collide on disk.
func modelDirName(modelID string, q catalog.Quant) string {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
				return r
			default:
				return '_'
			}
		}, s)
	}
	return safe(modelID) + "_" + safe(q.Name)
}

// vllmQuantScheme maps a quant to the vLLM --quantization scheme, or "" for a full-precision
// safetensors model vLLM loads without a scheme. The mapping is by the quant's name so a
// catalog entry does not have to carry vLLM's vocabulary.
func vllmQuantScheme(q catalog.Quant) string {
	name := strings.ToLower(q.Name)
	switch {
	case strings.Contains(name, "awq"):
		return "awq"
	case strings.Contains(name, "gptq"):
		return "gptq"
	case strings.Contains(name, "nvfp4"), strings.Contains(name, "fp4"):
		return "modelopt_fp4"
	case strings.Contains(name, "fp8"):
		return "fp8"
	default:
		return "" // full-precision safetensors
	}
}

// vllmMemMiB sizes the container's system-memory cap from the model's on-disk size plus
// headroom for the runtime, so a runaway is bounded without starving a legitimate load.
func vllmMemMiB(q catalog.Quant) int {
	const headroomMiB = 8192
	sizeMiB := int(q.SizeBytes >> 20)
	return sizeMiB + headroomMiB
}

// selectServeQuant picks the quantization to serve and the engine for it, preferring the
// smallest one this host can actually run. When the host has no GPU container path, a quant
// that needs vLLM is skipped, so a model with both a vLLM and a GGUF quant falls back to the
// GGUF/llama.cpp path rather than failing; a model with nothing serveable here is refused
// with a reason rather than started on a runtime it cannot run.
func selectServeQuant(m catalog.ModelSpec, gpuContainer bool) (catalog.Quant, launch.Engine, error) {
	var best catalog.Quant
	var bestEngine launch.Engine
	found := false
	for _, q := range m.Quants {
		eng, ok := launch.EngineForFormat(q.Format)
		if !ok {
			continue // an unservable (code-executing) format is never selected
		}
		if eng == launch.EngineVLLM && !gpuContainer {
			continue // needs the GPU container path this host lacks
		}
		if !found || q.SizeBytes < best.SizeBytes {
			best, bestEngine, found = q, eng, true
		}
	}
	if !found {
		return catalog.Quant{}, "", fmt.Errorf("%q has no quantization this host can serve (its vLLM quant needs a GPU plus a container engine, and it has no GGUF quant to fall back to)", m.ID)
	}
	return best, bestEngine, nil
}
