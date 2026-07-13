package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/harness"
)

func TestModelCommandsRequireAnID(t *testing.T) {
	cases := map[string]func([]string, string, *bytes.Buffer) error{
		"models run":     func(a []string, d string, o *bytes.Buffer) error { return runModelRun(a, d, o) },
		"models probe":   func(a []string, d string, o *bytes.Buffer) error { return runModelProbe(a, d, o) },
		"models use":     func(a []string, d string, o *bytes.Buffer) error { return runModelUse(a, d, o) },
		"models stop":    func(a []string, d string, o *bytes.Buffer) error { return runModelStop(a, d, o) },
		"models inspect": func(a []string, d string, o *bytes.Buffer) error { return runModelInspect(a, d, o) },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			for _, args := range [][]string{nil, {""}} {
				var out bytes.Buffer
				if err := run(args, t.TempDir(), &out); err == nil {
					t.Fatalf("%s with args %v: expected a required-id error", name, args)
				}
			}
		})
	}
}

// TestRunModelStopWithNothingRunning checks the command reports plainly when there is no
// server to stop, rather than failing.
func TestRunModelStopWithNothingRunning(t *testing.T) {
	var out bytes.Buffer
	if err := runModelStop([]string{"qwen2.5-coder:1.5b"}, t.TempDir(), &out); err != nil {
		t.Fatalf("runModelStop: %v", err)
	}
	if !strings.Contains(out.String(), "no running server found for qwen2.5-coder:1.5b") {
		t.Errorf("out = %q", out.String())
	}
}

// TestRunModelStatusReportsTheDefault checks the idle report: no servers running, and the
// recorded default model named with the command that would start it.
func TestRunModelStatusReportsTheDefault(t *testing.T) {
	dataDir := t.TempDir()

	var idle bytes.Buffer
	if err := runModelStatus(nil, dataDir, &idle); err != nil {
		t.Fatalf("runModelStatus: %v", err)
	}
	if !strings.Contains(idle.String(), "no local model servers are running.") {
		t.Fatalf("out = %q", idle.String())
	}
	if strings.Contains(idle.String(), "default model:") {
		t.Error("no default is recorded yet, so none should be reported")
	}

	if err := writeActiveModel(dataDir, "openai:gpt-5.5"); err != nil {
		t.Fatalf("writeActiveModel: %v", err)
	}
	var withDefault bytes.Buffer
	if err := runModelStatus(nil, dataDir, &withDefault); err != nil {
		t.Fatalf("runModelStatus: %v", err)
	}
	if !strings.Contains(withDefault.String(), "default model: openai:gpt-5.5") {
		t.Errorf("the recorded default must be reported, got %q", withDefault.String())
	}
}

// TestRunModelUseRecordsAProviderModel checks a hosted provider spec is recorded as the
// default without any local provisioning, while an id that is neither a catalog model nor a
// known provider is refused.
func TestRunModelUseRecordsAProviderModel(t *testing.T) {
	dataDir := t.TempDir()
	var out bytes.Buffer
	if err := runModelUse([]string{"openai:gpt-5.5"}, dataDir, &out); err != nil {
		t.Fatalf("runModelUse: %v", err)
	}
	got, ok := readActiveModel(dataDir)
	if !ok || got != "openai:gpt-5.5" {
		t.Fatalf("active model = %q (recorded: %v), want the provider spec", got, ok)
	}
	if !strings.Contains(out.String(), "is set as the default model") {
		t.Errorf("out = %q", out.String())
	}

	var bad bytes.Buffer
	if err := runModelUse([]string{"no-such-model"}, t.TempDir(), &bad); err == nil {
		t.Fatal("an unknown id must be refused")
	}
	// The refused selection must not have overwritten the recorded default.
	if got, _ := readActiveModel(dataDir); got != "openai:gpt-5.5" {
		t.Errorf("a failed selection changed the default to %q", got)
	}
}

func TestKnownProviderSpecRejectsBareNames(t *testing.T) {
	if knownProviderSpec("gpt-5.5") {
		t.Error("a bare model name is not a provider spec")
	}
	if knownProviderSpec(":x") {
		t.Error("an empty provider name is not a provider spec")
	}
	if knownProviderSpec("nosuchprovider:model") {
		t.Error("an unknown provider must not be accepted")
	}
}

func TestConsentFor(t *testing.T) {
	if consentFor(true) != consentPreapproved {
		t.Error("--yes must map to preapproved consent")
	}
	if consentFor(false) != consentPrompt {
		t.Error("the default must prompt for consent")
	}
}

// TestGateAndServeRefusesANonCatalogSourceWithoutConsent locks the admission gate: a hub
// source is never served without either an explicit --yes or an interactive confirmation,
// and the risk surface is shown before the refusal so it is explained rather than bare.
func TestGateAndServeRefusesANonCatalogSourceWithoutConsent(t *testing.T) {
	var out bytes.Buffer
	r := newLocalRunner(t.TempDir(), &out)
	defer func() { _ = r.Close() }()
	// The test process has no terminal, so this is a non-interactive session.
	_, _, err := r.gateAndServe(context.Background(), "hf:someone/unknown-model/model.gguf", consentPrompt, 0, false)
	if err == nil {
		t.Fatal("a non-catalog source must not be served without consent")
	}
	if out.Len() == 0 {
		t.Error("the risk surface must be shown before a refusal")
	}
}

// TestGateAndServeRefusesAnUnsafeWeightFormat checks the format guard fires on the
// reference alone: a pickle weight is refused before anything is fetched or run.
func TestGateAndServeRefusesAnUnsafeWeightFormat(t *testing.T) {
	r := newLocalRunner(t.TempDir(), &bytes.Buffer{})
	defer func() { _ = r.Close() }()
	_, _, err := r.gateAndServe(context.Background(), "hf:someone/model/pytorch_model.bin", consentPreapproved, 0, false)
	if err == nil {
		t.Fatal("a code-executing weight format must be refused")
	}
}

// TestLocalModelPlanIsConservativeWithoutAMeasurement checks an unmeasured model is driven
// with the safe scaffolding: no store on disk means no profile, which must never be read as
// "reliable".
func TestLocalModelPlanIsConservativeWithoutAMeasurement(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	if src := localProfileSource(ctx, dataDir); src == nil {
		t.Fatal("a missing store must still yield a profile source")
	}
	m := localTestModel()
	m.ContextTokens = 32768
	plan := localModelPlan(ctx, m, dataDir)
	if !plan.ConstrainToolCalls {
		t.Error("an unmeasured model must have its tool calls constrained")
	}
	if plan.MaxContext <= 0 || plan.MaxContext > m.ContextTokens {
		t.Errorf("plan context cap %d must be positive and within the advertised %d", plan.MaxContext, m.ContextTokens)
	}
}

func TestLogPlanReportsTheScaffolding(t *testing.T) {
	var out bytes.Buffer
	logPlan(&out, "qwen", harness.Plan{ConstrainToolCalls: true, SimplifyToolSchemas: true, VerifyPasses: 2, MaxContext: 8192})
	got := out.String()
	for _, want := range []string{"qwen", "constrain=true", "simplify=true", "verify=2", "maxContext=8192"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan line %q is missing %q", got, want)
		}
	}
}

func TestQuantLabelNamesAnUnrecordedQuant(t *testing.T) {
	if got := quantLabel(""); got != "default quant" {
		t.Errorf("quantLabel(\"\") = %q", got)
	}
	if got := quantLabel("Q4_K_M"); got != "Q4_K_M" {
		t.Errorf("quantLabel = %q", got)
	}
}

// TestDispatchModelsRoutesSubcommands checks the router reaches the handler for each verb:
// a verb whose handler validates its arguments returns that handler's error, and an unknown
// verb falls through to the catalog browser rather than failing.
func TestDispatchModelsRoutesSubcommands(t *testing.T) {
	dataDir := t.TempDir()
	for _, verb := range []string{"run", "probe", "use", "stop", "inspect", "bless", "fetch"} {
		if err := dispatchModels([]string{verb}, dataDir); err == nil {
			t.Errorf("models %s with no argument must report what it needs", verb)
		}
	}
	// The bare command and an unknown verb both browse the catalog.
	if err := dispatchModels(nil, dataDir); err != nil {
		t.Errorf("bare models command: %v", err)
	}
	if err := dispatchModels([]string{"qwen"}, dataDir); err != nil {
		t.Errorf("an unknown verb should browse the catalog, got %v", err)
	}
}
