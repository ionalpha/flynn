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
