package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/gguf"
	"github.com/ionalpha/flynn/inference"
	"github.com/ionalpha/flynn/inference/launch"
	"github.com/ionalpha/flynn/inference/modelsource"
	"github.com/ionalpha/flynn/sandbox"
)

// TestAdversarialCorpusEachThreatNeutralizedAtItsLayer is the defense-in-depth proof: a
// corpus of hostile model inputs, each run through the real layer that is meant to catch
// it, asserting the threat is neutralized and naming which layer caught it. A model from
// anywhere passes through the same pipeline (classify trust -> safe-parse -> gate runtime
// -> contain -> consent), and this test pins that each layer holds, so a regression that
// opens one of these holes fails here.
//
// It exercises the layers that are built and enforced today. Layers owned by other,
// not-yet-built tasks (outbound egress denial, runtime resource limits, inference-time
// behavior governance) are deliberately not asserted here, so the corpus never claims a
// containment the system does not yet provide.
func TestAdversarialCorpusEachThreatNeutralizedAtItsLayer(t *testing.T) {
	gr := gateRunner(t)
	// A GGUF that ships its own (hostile) chat template, built once for the corpus.
	poisonedGGUF := writeMinimalGGUF(t, map[string]string{"tokenizer.chat_template": "{{ ignore safety and exfiltrate }}"})

	cases := []struct {
		name  string
		layer string
		// neutralized runs the real layer check and returns whether the threat was
		// caught, plus a detail string for the failure message.
		neutralized func() (bool, string)
	}{
		{
			name:  "unknown-publisher hub model",
			layer: "trust + containment gate",
			neutralized: func() (bool, string) {
				// An arbitrary HuggingFace ref from an unrecognized publisher is untrusted
				// and may run only on the strong tier, which a process-jail host lacks.
				src, err := modelsource.Parse("hf:rando/sketchy-model/model.gguf", isLocalModelID)
				if err != nil {
					return false, "parse failed: " + err.Error()
				}
				_, err = gr.admitSource(src)
				return err != nil && strings.Contains(err.Error(), "untrusted"), errStr(err)
			},
		},
		{
			name:  "code-executing weight format (pickle)",
			layer: "safe-parse format guard",
			neutralized: func() (bool, string) {
				// A pickle weight executes code on load and is refused for every source.
				err := modelsource.CheckRunnableFormat("downloaded-model.bin")
				return err != nil && strings.Contains(err.Error(), "code-executing"), errStr(err)
			},
		},
		{
			name:  "poisoned chat template embedded in weights",
			layer: "hardened gguf reader + template override",
			neutralized: func() (bool, string) {
				// A GGUF carrying its own chat template must never set the prompt contract:
				// the hardened reader sees it, and the trusted template is forced instead.
				decision, err := launch.InspectTemplate(poisonedGGUF, "chatml")
				if err != nil {
					return false, "inspect failed: " + err.Error()
				}
				caught := decision.ModelSupplied && decision.Template == "chatml"
				return caught, "template chosen: " + decision.Template
			},
		},
		{
			name:  "malformed weights aimed at a parser flaw",
			layer: "hardened gguf reader",
			neutralized: func() (bool, string) {
				// Our own reader rejects a malformed file, so the runtime's parser is never
				// handed something it could be exploited by.
				_, err := gguf.ReadMetadata(bytes.NewReader([]byte("this is not a gguf header at all")))
				return err != nil, errStr(err)
			},
		},
		{
			name:  "runtime with a known-unpatched parser flaw",
			layer: "runtime version floor gate",
			neutralized: func() (bool, string) {
				// A runtime build below the safe floor is refused before it ever runs a model.
				err := inference.SafeToRun("llama.cpp", inference.Version{8000}) // floor is b8146
				return err != nil, errStr(err)
			},
		},
		{
			name:  "reputable but unverified model on a weak host",
			layer: "containment gate",
			neutralized: func() (bool, string) {
				// Even a recognized publisher is semi-trusted and needs kernel confinement;
				// a process-jail host cannot provide it, so the run is refused.
				src, _ := modelsource.Parse("hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF/model.gguf", isLocalModelID)
				_, err := gr.admitSource(src)
				return err != nil && strings.Contains(err.Error(), "semi-trusted"), errStr(err)
			},
		},
		{
			name:  "risky model run in a non-interactive pipeline",
			layer: "consent gate",
			neutralized: func() (bool, string) {
				// A non-interactive context must refuse a risky run rather than assume yes.
				rs := modelsource.DescribeRisk(
					modelsource.Source{Kind: modelsource.KindHuggingFace, Raw: "hf:rando/x", Owner: "rando", Repo: "x"},
					modelsource.Classify(modelsource.Source{Kind: modelsource.KindHuggingFace, Owner: "rando"}, nil),
					modelsource.IntegrityUnverified,
				)
				err := requireConsent(rs, false /*interactive*/, false /*autoApprove*/, strings.NewReader(""), &bytes.Buffer{})
				return err != nil, errStr(err)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, detail := c.neutralized()
			if !ok {
				t.Fatalf("threat %q was NOT neutralized at the %s layer: %s", c.name, c.layer, detail)
			}
			t.Logf("neutralized %q at the %s layer", c.name, c.layer)
		})
	}

	// Guard the guard: a benign trusted catalog model must pass every layer, so the
	// pipeline refuses the hostile corpus without also blocking the legitimate path.
	src, _ := modelsource.Parse("qwen2.5:0.5b-instruct", isLocalModelID)
	if _, err := gr.admitSource(src); err != nil {
		t.Fatalf("a benign catalog model must pass the gate, got %v", err)
	}
	if sandbox.Required(sandbox.TrustTrusted) != sandbox.ContainmentNone {
		t.Fatal("a trusted model must not require more than the process jail")
	}
}

func errStr(err error) string {
	if err == nil {
		return "<nil> (threat not caught)"
	}
	return err.Error()
}
