package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
)

// TestGateAndServeAdmitsACatalogModel checks the admitted path end to end: a vetted catalog
// model passes the gate, is recorded in the provenance ledger with its trust, and comes back
// as a running endpoint. The risk surface is printed for a prompted run because a user is
// entitled to see the posture even of a model that is admitted.
func TestGateAndServeAdmitsACatalogModel(t *testing.T) {
	stub := newStubModelServer(t, "ok", false)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)

	m, ep, err := r.gateAndServe(context.Background(), localCatalogModelID, consentPrompt, 0, false)
	if err != nil {
		t.Fatalf("gateAndServe: %v", err)
	}
	if m.ID != localCatalogModelID || ep.BaseURL == "" {
		t.Fatalf("gateAndServe returned %q at %q", m.ID, ep.BaseURL)
	}
	if !strings.Contains(out.String(), "trust:") {
		t.Errorf("the risk surface must be shown for a prompted run, got:\n%s", out.String())
	}

	recs, err := r.ledger.List()
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(recs) != 1 || !strings.Contains(recs[0].Raw, localCatalogModelID) || recs[0].Trust == "" {
		t.Errorf("the source's provenance must be recorded before it runs, got %+v", recs)
	}
}

// TestGateAndServeIsSilentUnderTheTrustGateOnlyPolicy checks the goal path's policy: the
// classification, the provenance record, and the containment gate still apply, but no risk
// surface is printed and no consent is asked for, so a goal run is not interrupted by a
// prompt for a model the catalog already vouches for.
func TestGateAndServeIsSilentUnderTheTrustGateOnlyPolicy(t *testing.T) {
	stub := newStubModelServer(t, "ok", false)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)

	if _, _, err := r.gateAndServe(context.Background(), localCatalogModelID, consentTrustGateOnly, 0, false); err != nil {
		t.Fatalf("gateAndServe: %v", err)
	}
	if strings.Contains(out.String(), "trust:") {
		t.Errorf("the trust-gate-only policy must print no risk surface, got:\n%s", out.String())
	}
	if recs, _ := r.ledger.List(); len(recs) != 1 {
		t.Errorf("provenance must still be recorded under the silent policy, got %+v", recs)
	}
}

// TestGateAndServeRefusesWhatItCannotContain is the security guarantee: a hub source is
// semi-trusted at best, which needs kernel-confined isolation, so a host whose sandbox
// provides less refuses to serve it even with an explicit approval. The refusal names the
// trust, the isolation the work needs, and what the host has, and the risk surface is shown
// before it so the refusal is explained rather than bare.
func TestGateAndServeRefusesWhatItCannotContain(t *testing.T) {
	var out bytes.Buffer
	r := newGatedRunner(t, 1, &out)

	_, _, err := r.gateAndServe(context.Background(), "hf:Qwen/Qwen2.5-7B-Instruct-GGUF/qwen2.5-7b.gguf", consentPreapproved, 0, false)
	if err == nil {
		t.Fatal("a non-catalog source must not be served on a host that cannot contain it")
	}
	// Either the containment gate refuses it, or (on a host that can contain it) serving a
	// non-catalog source is reported as not wired. Both are refusals, never a silent run.
	msg := err.Error()
	contained := strings.Contains(msg, "refusing to run it unsafely")
	notWired := strings.Contains(msg, "only catalog models serve today")
	if !contained && !notWired {
		t.Errorf("a non-catalog source must be refused with a reason, got %v", err)
	}
	if contained && !strings.Contains(msg, "isolation") {
		t.Errorf("a containment refusal must name the isolation it needs, got %v", err)
	}
	if !strings.Contains(out.String(), "trust:") {
		t.Errorf("the risk surface must be shown before the refusal, got:\n%s", out.String())
	}
}

// TestRiskSurfaceReportsIntegrity locks the integrity verdict the user is shown: a catalog
// model is pinned ahead of time, a source whose digest was pinned on first use is
// trust-on-first-use, and a source nothing is known about is unverified.
func TestRiskSurfaceReportsIntegrity(t *testing.T) {
	var out bytes.Buffer
	r := newGatedRunner(t, 1, &out)

	catSrc, err := modelsource.Parse(localCatalogModelID, isLocalModelID)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := r.riskSurface(catSrc, modelsource.Classify(catSrc, r.knownPublisher)).Integrity; got != modelsource.IntegrityPinned {
		t.Errorf("a catalog model's integrity = %v, want pinned", got)
	}

	hubSrc, err := modelsource.Parse("hf:Qwen/Qwen2.5-7B-Instruct-GGUF/qwen2.5-7b.gguf", isLocalModelID)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	class := modelsource.Classify(hubSrc, r.knownPublisher)
	if got := r.riskSurface(hubSrc, class).Integrity; got != modelsource.IntegrityUnverified {
		t.Errorf("an unpinned source's integrity = %v, want unverified", got)
	}

	// Pin the source's digest, as a first verified fetch would, and the verdict becomes
	// trust-on-first-use.
	if err := r.ledger.Record(modelsource.Provenance{
		Key:    hubSrc.Key(),
		Raw:    hubSrc.Raw,
		Trust:  class.Trust.String(),
		Digest: "sha256:" + strings.Repeat("aa", 32),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := r.riskSurface(hubSrc, class).Integrity; got != modelsource.IntegrityTOFU {
		t.Errorf("a pinned source's integrity = %v, want trust-on-first-use", got)
	}
}

// TestKnownPublisherCountsTheCatalogsOwn checks a publisher the curated catalog already
// vouches for is recognized on the hub, while an unknown owner is not.
func TestKnownPublisherRecognizesACatalogPublisher(t *testing.T) {
	r := newGatedRunner(t, 1, &bytes.Buffer{})
	pubs := catalogPublishers()
	if len(pubs) == 0 {
		t.Fatal("the embedded catalog must name at least one publisher")
	}
	if !r.knownPublisher(pubs[0]) {
		t.Errorf("%q is a catalog publisher but was not recognized", pubs[0])
	}
	if r.knownPublisher("some-random-uploader") {
		t.Error("an unknown owner must not be treated as a first-party publisher")
	}
}

// TestSourceFileNameNamesTheCheckableFile checks the format guard's input: only a reference
// that names a concrete file yields one, so a bare catalog id or hub repo is not
// format-checked on a name it does not have.
func TestSourceFileNameNamesTheCheckableFile(t *testing.T) {
	cases := map[string]string{
		"hf:Qwen/Qwen2.5-7B-GGUF/model.gguf": "model.gguf",
		localCatalogModelID:                  "",
	}
	for ref, want := range cases {
		src, err := modelsource.Parse(ref, isLocalModelID)
		if err != nil {
			t.Fatalf("parse %q: %v", ref, err)
		}
		if got := sourceFileName(src); got != want {
			t.Errorf("sourceFileName(%q) = %q, want %q", ref, got, want)
		}
	}
	if got := sourceFileName(modelsource.Source{Kind: modelsource.KindURL, URL: "https://example.test/w.gguf"}); got != "https://example.test/w.gguf" {
		t.Errorf("a URL source must be checked on its URL, got %q", got)
	}
	if got := sourceFileName(modelsource.Source{Kind: modelsource.KindFile, Path: "/w/model.gguf"}); got != "/w/model.gguf" {
		t.Errorf("a file source must be checked on its path, got %q", got)
	}
}

// TestRealEnsureWeightsRefusesAndReuses covers the weights step without any download: a
// code-executing quant is refused outright, a file already on disk is reused as it stands
// (it was verified when it was written), and a quant whose URL cannot be fetched over the
// hardened transport fails as a fetch error rather than writing a partial file.
func TestRealEnsureWeightsRefusesAndReuses(t *testing.T) {
	dataDir := t.TempDir()
	var out bytes.Buffer
	ensure := realEnsureWeights(dataDir, &out)
	m := localTestModel()

	pickle := catalog.Quant{Name: "fp16", Format: catalog.FormatPickle, URL: "https://example.test/w.bin"}
	if _, err := ensure(context.Background(), m, pickle); err == nil ||
		!strings.Contains(err.Error(), "code-executing weight format") {
		t.Fatalf("a pickle quant must be refused before it is fetched, got %v", err)
	}

	q := m.Quants[0]
	dest := filepath.Join(dataDir, "models", weightsFileName(m.ID, q))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("already verified"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ensure(context.Background(), m, q)
	if err != nil || got != dest {
		t.Fatalf("an existing file must be reused, got %q, %v", got, err)
	}
	if out.Len() != 0 {
		t.Errorf("reusing weights must not report a fetch, got %q", out.String())
	}

	// A quant that is not on disk is fetched, and the hardened transport refuses a
	// plaintext URL, so the failure is reported as a fetch failure with nothing written.
	insecure := catalog.Quant{Name: "Q8_0", Format: catalog.FormatGGUF, URL: "http://example.test/w.gguf", SizeBytes: 10}
	if _, err := ensure(context.Background(), m, insecure); err == nil || !strings.Contains(err.Error(), "fetch weights") {
		t.Fatalf("an unfetchable quant must fail as a fetch error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "models", weightsFileName(m.ID, insecure))); !os.IsNotExist(err) {
		t.Error("a failed fetch must leave no file behind")
	}
}

// TestRealEnsureRuntimeRefusesAnUnpinnedRuntime checks the provisioning step will only
// install a runtime this build pins a release for: an unknown engine name is refused rather
// than resolved at run time.
func TestRealEnsureRuntimeRefusesAnUnpinnedRuntime(t *testing.T) {
	var out bytes.Buffer
	ensure := realEnsureRuntime(t.TempDir(), &out)
	_, err := ensure(context.Background(), "not-a-runtime")
	if err == nil || !strings.Contains(err.Error(), "no pinned not-a-runtime build") {
		t.Fatalf("error = %v, want a refusal to provision an unpinned runtime", err)
	}
}

// TestResolveLocalModelRefusesAnUnknownSelection checks the goal path's resolver names the
// model it could not resolve, so a bad --model is explained rather than surfacing as a bare
// classification error.
func TestResolveLocalModelRefusesAnUnknownSelection(t *testing.T) {
	_, _, err := resolveLocalModel(context.Background(), "definitely-not-a-model", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "local model definitely-not-a-model") {
		t.Fatalf("error = %v, want the selection named", err)
	}
}

// TestLocalProfileSourceReadsARecordedMeasurement checks the plan input: with a store on disk
// the recorded profiles are read back, and with no store nothing is created just to look.
func TestLocalProfileSourceReadsARecordedMeasurement(t *testing.T) {
	dataDir := t.TempDir()
	if _, ok := localProfileSource(context.Background(), dataDir).Profile(localCatalogModelID); ok {
		t.Error("nothing has been measured, so no model may report a profile")
	}
	if _, err := os.Stat(dataStoreFile(dataDir)); !os.IsNotExist(err) {
		t.Error("a read-only profile lookup must not create the store")
	}

	// An in-memory data directory has no store file at all, which must still resolve to an
	// empty source rather than failing the run.
	if src := localProfileSource(context.Background(), ":memory:"); src == nil {
		t.Error("an ephemeral data dir must still yield a profile source")
	}
}

// TestCloseReleasesTheSandbox checks the runner's teardown is safe on every shape a command
// can hold: a nil runner, a runner with no sandbox, and a real one, which closes once.
func TestCloseReleasesTheSandbox(t *testing.T) {
	var nilRunner *localRunner
	if err := nilRunner.Close(); err != nil {
		t.Errorf("closing a nil runner must be harmless, got %v", err)
	}
	if err := (&localRunner{}).Close(); err != nil {
		t.Errorf("closing a runner with no sandbox must be harmless, got %v", err)
	}
	r := newLocalRunner(t.TempDir(), &bytes.Buffer{})
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestIsLocalModelIDRoutesTheGoalPath checks the predicate the goal path routes on: a local
// catalog id is local, a hosted catalog id is not, and an unknown id is not.
func TestIsLocalModelIDRoutesTheGoalPath(t *testing.T) {
	if !isLocalModelID("  " + localCatalogModelID + "  ") {
		t.Error("a local catalog id must be recognized, whitespace and all")
	}
	if isLocalModelID("definitely-not-a-model") {
		t.Error("an unknown id is not local")
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, m := range cat.Models {
		if !m.Local() {
			if isLocalModelID(m.ID) {
				t.Errorf("%q is a hosted model but was routed as local", m.ID)
			}
			break
		}
	}
}

// TestFindLocalModelRefusesAHostedModel checks a hosted catalog model is not resolvable as a
// local one: there is nothing to run locally, and the error says how to select it instead.
func TestFindLocalModelRefusesAHostedModel(t *testing.T) {
	hosted := hostedCatalogModelID(t)
	_, err := findLocalModel(hosted)
	if err == nil || !strings.Contains(err.Error(), "hosted API model") {
		t.Fatalf("error = %v, want a hosted-model refusal", err)
	}
	if !strings.Contains(err.Error(), "--model "+hosted) {
		t.Errorf("the refusal must name how to select it instead, got %v", err)
	}
}

// TestLocalProfileSourceToleratesAnUnreadableStore checks a profile lookup never blocks a run:
// a store that cannot be opened resolves every model as unmeasured, which yields the
// conservative plan rather than a failure.
func TestLocalProfileSourceToleratesAnUnreadableStore(t *testing.T) {
	dataDir := t.TempDir()
	// A directory where the database file should be: it exists, so the lookup tries to open
	// it, and it cannot be read as a database.
	if err := os.MkdirAll(dataStoreFile(dataDir), 0o750); err != nil {
		t.Fatal(err)
	}
	src := localProfileSource(context.Background(), dataDir)
	if src == nil {
		t.Fatal("an unreadable store must still yield a profile source")
	}
	if _, ok := src.Profile(localCatalogModelID); ok {
		t.Error("an unreadable store must report every model as unmeasured")
	}
}
