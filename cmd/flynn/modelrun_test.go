package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// fakeServeProc is a started-process stand-in for the runner tests.
type fakeServeProc struct{ done chan struct{} }

func (f *fakeServeProc) PID() int { return 4321 }
func (f *fakeServeProc) Running() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}
func (f *fakeServeProc) Output() string        { return "" }
func (f *fakeServeProc) Done() <-chan struct{} { return f.done }
func (f *fakeServeProc) Stop() error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

// fakeServeLauncher hands back a fake process and records the spec it served.
type fakeServeLauncher struct{ got sandbox.ServeSpec }

func (l *fakeServeLauncher) Serve(_ context.Context, spec sandbox.ServeSpec) (serve.Proc, error) {
	l.got = spec
	return &fakeServeProc{done: make(chan struct{})}, nil
}

func localTestModel() catalog.ModelSpec {
	return catalog.ModelSpec{
		ID:           "ollama:qwen2.5-coder:1.5b",
		Kind:         catalog.KindLocal,
		ChatTemplate: "chatml",
		Quants: []catalog.Quant{{
			Name:      "Q4_K_M",
			Format:    catalog.FormatGGUF,
			SizeBytes: 1000,
			URL:       "https://example.test/qwen.gguf",
			Digest:    "sha256:abc",
		}},
	}
}

// newFakeRunner builds a localRunner whose provisioning steps are stubbed and whose
// manager serves through a fake launcher with an always-healthy probe, so the full
// lifecycle runs with no network and no real runtime.
func newFakeRunner(t *testing.T, weightsPath string) (*localRunner, *fakeServeLauncher) {
	t.Helper()
	launcher := &fakeServeLauncher{}
	reg := serve.NewRegistry(t.TempDir())
	mgr := serve.NewManager(
		launcher,
		func(context.Context, string) error { return nil }, // always healthy
		func(int) error { return nil },
		reg,
	)
	r := &localRunner{
		dataDir:       t.TempDir(),
		out:           &bytes.Buffer{},
		ensureRuntime: func(context.Context, string) (string, error) { return "/runtimes/llama-server", nil },
		ensureWeights: func(context.Context, catalog.ModelSpec, catalog.Quant) (string, error) { return weightsPath, nil },
		manager:       mgr,
		freePort:      func() (int, error) { return 8765, nil },
	}
	return r, launcher
}

func TestServeModelHappyPath(t *testing.T) {
	weights := writeMinimalGGUF(t, map[string]string{"general.architecture": "qwen2"})
	r, launcher := newFakeRunner(t, weights)

	ep, err := r.serveModel(context.Background(), localTestModel(), 0, false)
	if err != nil {
		t.Fatalf("serveModel: %v", err)
	}
	if ep.BaseURL != "http://127.0.0.1:8765/v1" {
		t.Fatalf("endpoint base URL = %q, want loopback :8765", ep.BaseURL)
	}
	// The served command must bind loopback and force the trusted template, never the
	// model's own, and must be confined.
	argv := launcher.got.Argv
	if !containsArg(argv, "--host", "127.0.0.1") {
		t.Fatalf("server not bound to loopback: %v", argv)
	}
	if !containsArg(argv, "--chat-template", "chatml") {
		t.Fatalf("trusted chat template not forced: %v", argv)
	}
	if !launcher.got.Confine {
		t.Fatal("the runtime must be served confined")
	}
}

func TestServeModelRejectsHostedModel(t *testing.T) {
	r, _ := newFakeRunner(t, "")
	_, err := r.serveModel(context.Background(), catalog.ModelSpec{ID: "openai:gpt-5.5", Kind: catalog.KindAPI}, 0, false)
	if err == nil {
		t.Fatal("expected an error serving a hosted API model locally")
	}
}

func TestServeModelRejectsNoDirectDownload(t *testing.T) {
	r, _ := newFakeRunner(t, "")
	m := localTestModel()
	m.Quants[0].URL = "" // ollama-style ref with no pinned direct download
	_, err := r.serveModel(context.Background(), m, 0, false)
	if err == nil || !contains(err.Error(), "no pinned direct download") {
		t.Fatalf("expected a no-direct-download error, got %v", err)
	}
}

func TestServeModelPropagatesProvisionError(t *testing.T) {
	weights := writeMinimalGGUF(t, nil)
	r, _ := newFakeRunner(t, weights)
	r.ensureRuntime = func(context.Context, string) (string, error) { return "", errors.New("gate refused vulnerable build") }
	_, err := r.serveModel(context.Background(), localTestModel(), 0, false)
	if err == nil || !contains(err.Error(), "gate refused") {
		t.Fatalf("expected the provision error to propagate, got %v", err)
	}
}

func containsArg(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
