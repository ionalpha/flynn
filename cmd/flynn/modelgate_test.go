package main

import (
	"bytes"
	"testing"

	"github.com/ionalpha/flynn/inference/modelsource"
	"github.com/ionalpha/flynn/sandbox"
)

// gateRunner builds a localRunner with a real (unconfined, process-jail) sandbox and a
// ledger, enough to exercise the trust gate and provenance recording without any serving.
func gateRunner(t *testing.T) *localRunner {
	t.Helper()
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return &localRunner{
		out:    &bytes.Buffer{},
		sb:     sb,
		ledger: modelsource.NewLedger(t.TempDir()),
	}
}

func TestAdmitSourceCatalogTrustedPasses(t *testing.T) {
	r := gateRunner(t)
	src := modelsource.Source{Kind: modelsource.KindCatalog, Raw: "qwen2.5:0.5b-instruct", CatalogID: "qwen2.5:0.5b-instruct"}
	class, err := r.admitSource(src)
	if err != nil {
		t.Fatalf("a catalog model must pass the gate on any host, got %v", err)
	}
	if class.Trust != sandbox.TrustTrusted {
		t.Fatalf("catalog trust = %v, want trusted", class.Trust)
	}
	// Provenance must have been recorded.
	if _, ok, _ := r.ledger.Get(src.Key()); !ok {
		t.Fatal("admitSource must record provenance")
	}
}

func TestAdmitSourceUntrustedRefusedOnWeakHost(t *testing.T) {
	r := gateRunner(t) // process-jail only, no kernel/microvm tier
	// An unknown-publisher hub model is untrusted and needs the strong tier; the
	// process-jail host cannot provide it, so it must be refused with a clear reason.
	src := modelsource.Source{Kind: modelsource.KindHuggingFace, Raw: "hf:rando/x", Owner: "rando", Repo: "x"}
	_, err := r.admitSource(src)
	if err == nil {
		t.Fatal("an untrusted model on a process-jail host must be refused")
	}
	if !contains(err.Error(), "untrusted") || !contains(err.Error(), "refusing") {
		t.Fatalf("refusal must name the trust level and refuse, got %q", err.Error())
	}
}

func TestAdmitSourceSemiTrustedRefusedWithoutKernelTier(t *testing.T) {
	r := gateRunner(t)
	// A recognized publisher is semi-trusted and needs kernel confinement; a process-jail
	// host does not provide it, so even a reputable hub model is refused here.
	src := modelsource.Source{Kind: modelsource.KindHuggingFace, Raw: "hf:Qwen/x", Owner: "Qwen", Repo: "x"}
	_, err := r.admitSource(src)
	if err == nil || !contains(err.Error(), "semi-trusted") {
		t.Fatalf("a semi-trusted model must be refused on a process-jail host, got %v", err)
	}
}

func TestAdmitSourceRefusesCodeExecFormat(t *testing.T) {
	r := gateRunner(t)
	// A dropped pickle file is refused on format grounds before the gate, for any source.
	src := modelsource.Source{Kind: modelsource.KindFile, Raw: "/tmp/model.bin", Path: "/tmp/model.bin"}
	_, err := r.admitSource(src)
	if err == nil || !contains(err.Error(), "code-executing") {
		t.Fatalf("a code-executing weight format must be refused, got %v", err)
	}
}
