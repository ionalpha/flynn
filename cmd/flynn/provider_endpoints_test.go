package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// The tests in this package must not probe the developer's own Ollama or LM Studio, so
// discovery finds nothing unless a test installs a fake.
func init() {
	localDiscovery = func(context.Context) []provider.LocalModel { return nil }
}

func fakeDiscovery(t *testing.T, models ...provider.LocalModel) {
	t.Helper()
	prev := localDiscovery
	localDiscovery = func(context.Context) []provider.LocalModel { return models }
	t.Cleanup(func() { localDiscovery = prev })
}

func TestModelsListsServerModelsAsLocalAndUnverified(t *testing.T) {
	fakeDiscovery(t,
		provider.LocalModel{Server: "ollama", ID: "gpt-oss:20b", Params: "20.9B", Quant: "MXFP4", Tools: true},
		provider.LocalModel{Server: "lmstudio", ID: "qwen3-coder-30b"},
	)
	var out bytes.Buffer
	if err := runModels(nil, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"unverified", "ollama:gpt-oss:20b", "20.9B MXFP4", "lmstudio:qwen3-coder-30b", "flynn goal --model ollama:gpt-oss:20b"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "ollama:gpt-oss:20b") && (!strings.Contains(line, "local") || !strings.Contains(line, "yes")) {
			t.Errorf("row should say local with tools yes: %q", line)
		}
		if strings.HasPrefix(line, "lmstudio:qwen3") && !strings.Contains(line, "unknown") {
			t.Errorf("a server that reports no capabilities should say unknown: %q", line)
		}
	}
}

func TestModelsOmitsServerModelsWhenFilteredOrAPIOnly(t *testing.T) {
	fakeDiscovery(t, provider.LocalModel{Server: "ollama", ID: "gpt-oss:20b"})
	for _, args := range [][]string{{"--api"}, {"--cap", "tools"}, {"--max-params", "70"}, {"--publisher", "meta"}, {"--safe"}, {"--max-size", "100"}} {
		var out bytes.Buffer
		_ = runModels(args, t.TempDir(), &out)
		if strings.Contains(out.String(), "gpt-oss:20b") {
			t.Errorf("%v: unverified server models cannot satisfy this filter:\n%s", args, out.String())
		}
	}
}

func TestModelsListingHasNoServerSectionWhenNothingRuns(t *testing.T) {
	var out bytes.Buffer
	if err := runModels(nil, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "running on this machine") {
		t.Fatalf("no server reported anything:\n%s", out.String())
	}
}

// TestModelsHelpNamesEveryBaseURLVariableAndTheProvidersFile is the documentation
// check: the help is the one place an operator learns how to point Flynn at a server.
func TestModelsHelpNamesEveryBaseURLVariableAndTheProvidersFile(t *testing.T) {
	var out bytes.Buffer
	if err := runModels([]string{"-h"}, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	for _, v := range append(provider.BaseURLEnvVars(), provider.VisionEnvVars()...) {
		if !strings.Contains(out.String(), v) {
			t.Errorf("help does not name %s", v)
		}
	}
	for _, want := range []string{provider.ProvidersFile, "allow_hosts", "base_url", "--model <name>:<model>"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help does not describe %q", want)
		}
	}
}

func TestWithProviderEndpoints(t *testing.T) {
	base := secret.EnvSource{}

	same, err := withProviderEndpoints(base, t.TempDir())
	if err != nil || same != secret.Source(base) {
		t.Fatalf("no file should leave the source as it is: %v %v", same, err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, provider.ProvidersFile), []byte(`{"providers":{"box":{"kind":"ollama","base_url":"http://localhost:11434/v1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := withProviderEndpoints(base, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ResolveWith(context.Background(), "box:gpt-oss:20b", src); err != nil {
		t.Fatalf("a configured name should resolve as --model box:<model>: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, provider.ProvidersFile), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := withProviderEndpoints(base, dir); err == nil {
		t.Fatal("a malformed providers file must be reported, not ignored")
	}
}

// TestDiscoverEnvLocalReadsTheEnvironment runs the real discovery against addresses
// nothing listens on, so it finds nothing and never reaches the developer's own servers.
func TestDiscoverEnvLocalReadsTheEnvironment(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("LMSTUDIO_BASE_URL", "http://127.0.0.1:1/v1")
	if got := discoverEnvLocal(context.Background()); len(got) != 0 {
		t.Fatalf("no server is listening, got %+v", got)
	}
}

func writeProvidersFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, provider.ProvidersFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const boxServer = `{"providers":{"box":{"kind":"ollama","base_url":"http://localhost:11434/v1"}}}`

// TestResolveModelReadsTheProvidersFile is the wiring check: --model <name>:<model>
// reaches a server declared in the data directory, and without the file the same
// spec is an unknown provider.
func TestResolveModelReadsTheProvidersFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, err := resolveModel(ctx, "box:gpt-oss:20b", dir); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("without a providers file box is not a provider: %v", err)
	}
	writeProvidersFile(t, dir, boxServer)
	if _, _, err := resolveModel(ctx, "box:gpt-oss:20b", dir); err != nil {
		t.Fatalf("a configured name should resolve: %v", err)
	}
	writeProvidersFile(t, dir, `not json`)
	if _, _, err := resolveModel(ctx, "box:gpt-oss:20b", dir); err == nil {
		t.Fatal("a malformed providers file must be reported, not ignored")
	}
}

func TestModelUseRecordsAConfiguredServer(t *testing.T) {
	dir := t.TempDir()
	if err := runModelUse([]string{"box:m"}, dir, &bytes.Buffer{}); err == nil {
		t.Fatal("box is not configured yet, so the id is unknown")
	}
	writeProvidersFile(t, dir, boxServer)
	var out bytes.Buffer
	if err := runModelUse([]string{"box:gpt-oss:20b"}, dir, &out); err != nil {
		t.Fatal(err)
	}
	if got, ok := readActiveModel(dir); !ok || got != "box:gpt-oss:20b" {
		t.Fatalf("active model = %q (recorded: %v)", got, ok)
	}
	writeProvidersFile(t, dir, `not json`)
	if err := runModelUse([]string{"box:m"}, dir, &out); err == nil || !strings.Contains(err.Error(), provider.ProvidersFile) {
		t.Fatalf("a malformed providers file should be named in the error: %v", err)
	}
}

func TestKnownProviderSpecAcceptsConfiguredServers(t *testing.T) {
	if !knownProviderSpec("box:m", []string{"box"}) {
		t.Error("a configured server name is a provider")
	}
	if knownProviderSpec("box:m", []string{"other"}) {
		t.Error("a name that is not configured must not be accepted")
	}
	if knownProviderSpec(":m", []string{""}) {
		t.Error("an empty name is never a provider")
	}
}

func TestConfiguredServerNames(t *testing.T) {
	dir := t.TempDir()
	if got, err := configuredServerNames(dir); err != nil || len(got) != 0 {
		t.Fatalf("no file: %v %v", got, err)
	}
	writeProvidersFile(t, dir, `{"providers":{"b":{"kind":"ollama","base_url":"http://localhost:1/v1"},"a":{"kind":"lmstudio","base_url":"http://localhost:2/v1"}}}`)
	if got, err := configuredServerNames(dir); err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v, %v; want a,b", got, err)
	}
	writeProvidersFile(t, dir, `not json`)
	if _, err := configuredServerNames(dir); err == nil {
		t.Fatal("a malformed file must be reported")
	}
}

// TestModelFlagDefaultIsTheProviderDefault keeps the --model default in step with the
// model a bare "anthropic" resolves to, so changing one cannot leave the other behind.
func TestModelFlagDefaultIsTheProviderDefault(t *testing.T) {
	got := runCLI(t, "-h")
	if want := `(default "` + provider.CanonicalSpec("anthropic") + `")`; !strings.Contains(got.stderr, want) {
		t.Fatalf("--model default is not %s:\n%s", want, got.stderr)
	}
}
