package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/hardware"
)

// TestProbeAndStoreWarnsBelowTheQuantFloor checks the caveat a user needs to read a score: a
// model served at a quantization too coarse for its size is measured, recorded, and reported
// with a warning that the score reflects the quant as much as the model.
func TestProbeAndStoreWarnsBelowTheQuantFloor(t *testing.T) {
	var out bytes.Buffer
	spec := catalog.ModelSpec{
		ID:            "ollama:tiny",
		ParamsB:       1.5,
		ContextTokens: 4096,
		Quants:        []catalog.Quant{{Name: "Q2_K", SizeBytes: 100}},
	}
	if err := probeAndStore(t.Context(), constModel{text: "ok"}, spec, selfProvisionedRuntime, profileStore(t), &out); err != nil {
		t.Fatalf("probeAndStore: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Q2_K") {
		t.Errorf("the served quant must be named in the report, got:\n%s", got)
	}
	if !strings.Contains(got, "warning:") && !strings.Contains(got, "note:") {
		t.Errorf("a coarse quant must be reported as a caveat on the score, got:\n%s", got)
	}
}

// TestWriteActiveModelReportsAnUnwritableDataDir checks a default that cannot be recorded is an
// error rather than a silently forgotten selection.
func TestWriteActiveModelReportsAnUnwritableDataDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeActiveModel(file, "openai:gpt-5.5"); err == nil {
		t.Fatal("recording a default under a file must fail")
	}
	if _, ok := readActiveModel(file); ok {
		t.Error("no default may be reported from a data dir that could not be written")
	}
}

// TestDispatchModelsReachesEveryVerb checks the router: each verb reaches a handler that
// validates its own arguments (so a bare verb reports what it needs), and an unknown verb
// browses the catalog rather than failing.
func TestDispatchModelsReachesEveryVerb(t *testing.T) {
	dataDir := t.TempDir()
	for _, args := range [][]string{{"search", ""}, {"pool"}, {"install", "not-a-runtime"}} {
		if err := dispatchModels(args, dataDir); err == nil {
			t.Errorf("models %v must report what it needs", args)
		}
	}
	// `models status` answers with the idle report rather than an error when nothing runs.
	if err := dispatchModels([]string{"status"}, dataDir); err != nil {
		t.Errorf("models status: %v", err)
	}
}

// TestServeModelRefusesUnreadableWeights checks the serve path reads the chat template out of
// the weights with the hardened reader before it starts anything: a file that is not a weights
// file at all is refused, so a corrupt or substituted download never reaches the runtime.
func TestServeModelRefusesUnreadableWeights(t *testing.T) {
	notWeights := filepath.Join(t.TempDir(), "weights.gguf")
	if err := os.WriteFile(notWeights, []byte("this is not a gguf file"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := newFakeRunner(t, notWeights)
	_, err := r.serveModel(context.Background(), localTestModel(), 0, false)
	if err == nil || !strings.Contains(err.Error(), "inspect weights before serving") {
		t.Fatalf("error = %v, want the weights inspection to refuse the file", err)
	}
}

// TestServeModelReportsAPortFailure checks a host that cannot hand out a loopback port fails
// with a clear reason instead of starting a server on an unknown address.
func TestServeModelReportsAPortFailure(t *testing.T) {
	weights := writeMinimalGGUF(t, map[string]string{"general.architecture": "qwen2"})
	r, _ := newFakeRunner(t, weights)
	r.freePort = func() (int, error) { return 0, errors.New("no free port") }
	_, err := r.serveModel(context.Background(), localTestModel(), 0, false)
	if err == nil || !strings.Contains(err.Error(), "pick a loopback port") {
		t.Fatalf("error = %v, want a port failure", err)
	}
}

// TestServeContainerModelRefusesAHostWithoutTheGPUPath checks the vLLM path never half-starts:
// a host with no GPU container path is refused with the reason, and a quant with no file
// manifest to fetch is refused even where the path exists.
func TestServeContainerModelRefusesAHostWithoutTheGPUPath(t *testing.T) {
	r, _ := newFakeRunner(t, "")
	m := catalog.ModelSpec{ID: "vllm:test", Kind: catalog.KindLocal}
	q := catalog.Quant{Name: "awq", Format: catalog.FormatSafetensors}

	_, err := r.serveContainerModel(context.Background(), m, q, 0)
	if err == nil || !strings.Contains(err.Error(), "needs the vLLM GPU runtime") {
		t.Fatalf("error = %v, want a refusal naming the missing GPU path", err)
	}

	// With the GPU path present, a quant with no file manifest still cannot be fetched.
	r.box = hardware.Box{
		VRAMBytes:  24 << 30,
		Containers: hardware.ContainerSupport{Docker: true, NVIDIAToolkit: true},
	}
	_, err = r.serveContainerModel(context.Background(), m, q, 0)
	if err == nil || !strings.Contains(err.Error(), "no file manifest to fetch") {
		t.Fatalf("error = %v, want a refusal naming the missing manifest", err)
	}
}

// TestEnsureModelDirReusesACompleteDirectory checks a re-run does no network: a model directory
// whose files are all present is reused as it stands.
func TestEnsureModelDirReusesACompleteDirectory(t *testing.T) {
	r, _ := newFakeRunner(t, "")
	m := catalog.ModelSpec{ID: "vllm:qwen2.5", Kind: catalog.KindLocal}
	q := catalog.Quant{
		Name: "awq",
		Files: []catalog.QuantFile{
			{Name: "config.json", URL: "https://example.test/config.json", Digest: "sha256:aa", SizeBytes: 2},
			{Name: "model.safetensors", URL: "https://example.test/model.safetensors", Digest: "sha256:bb", SizeBytes: 2},
		},
	}
	dir := filepath.Join(r.dataDir, "models", modelDirName(m.ID, q))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range q.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.ensureModelDir(context.Background(), m, q)
	if err != nil {
		t.Fatalf("ensureModelDir: %v", err)
	}
	if got != dir {
		t.Errorf("model dir = %q, want the complete directory reused at %q", got, dir)
	}
}

// TestVLLMMemMiBLeavesHeadroom checks the container's memory cap is sized from the model plus
// runtime headroom, so a load is bounded without starving a legitimate one.
func TestVLLMMemMiBLeavesHeadroom(t *testing.T) {
	got := vllmMemMiB(catalog.Quant{SizeBytes: 4 << 30})
	if got != 4096+8192 {
		t.Errorf("vllmMemMiB = %d, want the model size plus headroom", got)
	}
	if empty := vllmMemMiB(catalog.Quant{}); empty != 8192 {
		t.Errorf("a model with no recorded size still needs runtime headroom, got %d", empty)
	}
}

// TestRunModelInspectExplainsWhatAHostWouldDo checks `models inspect`: it classifies a
// reference and says, without fetching or running anything, what the weight format is and
// whether this host would run it. A source from an unknown uploader is reported as one this
// host would refuse, and a code-executing weight format is named as refused outright.
func TestRunModelInspectExplainsWhatAHostWouldDo(t *testing.T) {
	var unknown bytes.Buffer
	if err := runModelInspect([]string{"hf:some-uploader/mystery-model/weights.gguf"}, t.TempDir(), &unknown); err != nil {
		t.Fatalf("runModelInspect: %v", err)
	}
	got := unknown.String()
	if !strings.Contains(got, "format:    a safe-parse weight format") {
		t.Errorf("a gguf reference must be named as a safe-parse format, got:\n%s", got)
	}
	if !strings.Contains(got, "this host:") {
		t.Errorf("inspect must say what this host would do, got:\n%s", got)
	}

	var pickle bytes.Buffer
	if err := runModelInspect([]string{"hf:some-uploader/mystery-model/pytorch_model.bin"}, t.TempDir(), &pickle); err != nil {
		t.Fatalf("runModelInspect: %v", err)
	}
	if !strings.Contains(pickle.String(), "format:    refused") {
		t.Errorf("a code-executing weight format must be reported as refused, got:\n%s", pickle.String())
	}

	// A catalog model is the one case that runs without an explicit approval.
	var vetted bytes.Buffer
	if err := runModelInspect([]string{localCatalogModelID}, t.TempDir(), &vetted); err != nil {
		t.Fatalf("runModelInspect: %v", err)
	}
	if !strings.Contains(vetted.String(), "this host: runs it (a vetted catalog model)") {
		t.Errorf("a vetted catalog model must be reported as runnable, got:\n%s", vetted.String())
	}
}
