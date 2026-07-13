package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/orchestrate"
)

// hostedCatalogModelID names a catalog model that is served by a hosted API, so there is
// nothing to download or run locally. It is the id the local-only commands must refuse.
func hostedCatalogModelID(t *testing.T) string {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, m := range cat.Models {
		if !m.Local() {
			return m.ID
		}
	}
	t.Skip("the catalog has no hosted model to check the refusal against")
	return ""
}

// TestRunModelFetchRefusesBeforeItDownloads covers every refusal the fetch command makes
// before it touches the network: no id, an id that is not in the catalog, a hosted model with
// nothing to download, and a quantization the model does not publish.
func TestRunModelFetchRefusesBeforeItDownloads(t *testing.T) {
	hosted := hostedCatalogModelID(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no id", nil, "a model id is required"},
		{"unknown id", []string{"no-such-model"}, "is not in the catalog"},
		{"hosted model", []string{hosted}, "is a hosted API model"},
		{"unknown quant", []string{"--quant", "Q9_XXL", localCatalogModelID}, "has no quantization"},
		{"bad flag", []string{"--nope", localCatalogModelID}, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runModelFetch(c.args, t.TempDir(), &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// TestPickQuantPrefersTheSmallest checks the default choice and the named one: with no name
// the smallest quantization is served, a name is matched case-insensitively, and an unknown
// name is refused rather than falling back to something the user did not ask for.
func TestPickQuantPrefersTheSmallest(t *testing.T) {
	m := catalog.ModelSpec{Quants: []catalog.Quant{
		{Name: "Q8_0", SizeBytes: 900},
		{Name: "Q4_K_M", SizeBytes: 300},
	}}
	q, ok := pickQuant(m, "")
	if !ok || q.Name != "Q4_K_M" {
		t.Errorf("default quant = %q (%v), want the smallest", q.Name, ok)
	}
	if q, ok := pickQuant(m, "q8_0"); !ok || q.Name != "Q8_0" {
		t.Errorf("named quant = %q (%v), want a case-insensitive match", q.Name, ok)
	}
	if _, ok := pickQuant(m, "Q2_K"); ok {
		t.Error("a quantization the model does not publish must not be substituted")
	}
}

// TestRunRuntimeInstallRefusesAnUnpinnedRuntime checks the install command will only place a
// runtime this build pins a verified release for, and names what it can provision instead.
func TestRunRuntimeInstallRefusesAnUnpinnedRuntime(t *testing.T) {
	var out bytes.Buffer
	err := runRuntimeInstall([]string{"not-a-runtime"}, t.TempDir(), &out)
	if err == nil || !strings.Contains(err.Error(), "no pinned not-a-runtime build") {
		t.Fatalf("error = %v, want a refusal to install an unpinned runtime", err)
	}
	names := installableRuntimes()
	if len(names) == 0 {
		t.Fatal("the build must pin at least one installable runtime")
	}
	for _, n := range names {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the refusal must name %q as installable, got %v", n, err)
		}
	}
}

// TestRunModelPoolRefusesAnUnrunnablePool covers what the pool command validates before it
// starts reconciling: it needs at least one model, every id must be a known local model, and a
// bad flag is reported rather than ignored.
func TestRunModelPoolRefusesAnUnrunnablePool(t *testing.T) {
	hosted := hostedCatalogModelID(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no models", nil, "at least one model id is required"},
		{"unknown model", []string{"no-such-model"}, "is not in the catalog"},
		{"hosted model", []string{hosted}, "is a hosted API model"},
		{"bad flag", []string{"--nope"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runModelPool(c.args, t.TempDir(), &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// TestLaunchProfileShrinksARecoveryLaunch checks the recovery ladder: a degraded launch
// tightens the context window, a minimal launch tightens it further and drops to the CPU, and
// a full launch leaves the runtime's defaults alone.
func TestLaunchProfileShrinksARecoveryLaunch(t *testing.T) {
	cases := []struct {
		level   orchestrate.LaunchLevel
		ctxSize int
		cpuOnly bool
	}{
		{orchestrate.LaunchFull, 0, false},
		{orchestrate.LaunchDegraded, degradedContextTokens, false},
		{orchestrate.LaunchMinimal, minimalContextTokens, true},
	}
	for _, c := range cases {
		ctxSize, cpuOnly := launchProfile(c.level)
		if ctxSize != c.ctxSize || cpuOnly != c.cpuOnly {
			t.Errorf("launchProfile(%v) = (%d, %v), want (%d, %v)", c.level, ctxSize, cpuOnly, c.ctxSize, c.cpuOnly)
		}
	}
	if degradedContextTokens <= minimalContextTokens {
		t.Error("a minimal launch must tighten the window further than a degraded one")
	}
}

// TestPoolLauncherGatesEveryLaunch checks a pool member is served through the same admission
// gate as `models run`, and a model the pool does not manage is refused rather than started.
func TestPoolLauncherGatesEveryLaunch(t *testing.T) {
	stub := newStubModelServer(t, "ok", false)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)
	m, err := findLocalModel(localCatalogModelID)
	if err != nil {
		t.Fatalf("findLocalModel: %v", err)
	}
	launch := poolLauncher(r, map[string]catalog.ModelSpec{m.ID: m})

	if err := launch(context.Background(), m.ID, orchestrate.LaunchFull); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The gate ran silently: a pool launch shows no risk surface and asks for no consent.
	if strings.Contains(out.String(), "trust:") {
		t.Errorf("a pool launch must not print a consent surface, got:\n%s", out.String())
	}
	if recs, _ := r.ledger.List(); len(recs) != 1 {
		t.Errorf("a pool launch must still record the source's provenance, got %+v", recs)
	}
	// A launch is idempotent: the already-running server is reused.
	if err := launch(context.Background(), m.ID, orchestrate.LaunchDegraded); err != nil {
		t.Fatalf("relaunch: %v", err)
	}

	err = launch(context.Background(), "not-in-the-pool", orchestrate.LaunchFull)
	if err == nil || !strings.Contains(err.Error(), "is not in the pool") {
		t.Fatalf("error = %v, want a refusal to launch a model the pool does not manage", err)
	}
}
