package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ionalpha/flynn/provider"
)

func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
			{"name":"gpt-oss:20b","details":{"parameter_size":"20.9B","quantization_level":"MXFP4"}},
			{"name":"bare:latest"},
			{"name":""}]}`))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case "gpt-oss:20b":
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools"]}`))
		case "bare:latest":
			_, _ = w.Write([]byte(`{"details":{"parameter_size":"7B","quantization_level":"Q4_K_M"},"capabilities":["completion"]}`))
		}
	})
	return httptest.NewServer(mux)
}

func TestDiscoverLocalOllama(t *testing.T) {
	srv := fakeOllama(t)
	defer srv.Close()

	got := provider.DiscoverLocal(context.Background(), mapSource{"OLLAMA_BASE_URL": srv.URL + "/v1", "LMSTUDIO_BASE_URL": "http://127.0.0.1:1/v1"}, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 models, got %+v", got)
	}
	a, b := got[0], got[1]
	if a.Spec() != "ollama:gpt-oss:20b" || a.Params != "20.9B" || a.Quant != "MXFP4" || !a.Tools {
		t.Errorf("first model: %+v", a)
	}
	if b.Spec() != "ollama:bare:latest" || b.Params != "7B" || b.Quant != "Q4_K_M" || b.Tools {
		t.Errorf("second model should take its size from /api/show and report no tools: %+v", b)
	}
	if a.String() != "ollama:gpt-oss:20b (20.9B, MXFP4)" {
		t.Errorf("String() = %q", a.String())
	}
	if (provider.LocalModel{Server: "lmstudio", ID: "x"}).String() != "lmstudio:x" {
		t.Error("a model with no size renders as its spec")
	}
}

func TestDiscoverLocalLMStudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-coder-30b"},{"id":""}]}`))
	}))
	defer srv.Close()

	got := provider.DiscoverLocal(context.Background(), mapSource{"LMSTUDIO_BASE_URL": srv.URL + "/v1", "OLLAMA_BASE_URL": "http://127.0.0.1:1/v1"}, nil)
	if len(got) != 1 || got[0].Spec() != "lmstudio:qwen3-coder-30b" {
		t.Fatalf("got %+v", got)
	}
}

// TestDiscoverLocalAbsenceIsNotAnError covers the normal case on a machine with no
// local server, and a port that answers with something that is not a model list.
func TestDiscoverLocalAbsenceIsNotAnError(t *testing.T) {
	down := mapSource{"OLLAMA_BASE_URL": "http://127.0.0.1:1/v1", "LMSTUDIO_BASE_URL": "http://127.0.0.1:1/v1"}
	if got := provider.DiscoverLocal(context.Background(), down, nil); len(got) != 0 {
		t.Fatalf("no server running should list nothing, got %+v", got)
	}

	for name, h := range map[string]http.HandlerFunc{
		"error status": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		"not json":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			src := mapSource{"OLLAMA_BASE_URL": srv.URL + "/v1", "LMSTUDIO_BASE_URL": srv.URL + "/v1"}
			if got := provider.DiscoverLocal(context.Background(), src, nil); len(got) != 0 {
				t.Fatalf("an unexpected answer should list nothing, got %+v", got)
			}
		})
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("no network in this test")
}

// TestDiscoverLocalProbesOnlySafeBaseURLs checks discovery never contacts a plaintext
// remote host that a run would refuse to use.
func TestDiscoverLocalProbesOnlySafeBaseURLs(t *testing.T) {
	rt := &countingTransport{}
	src := mapSource{"OLLAMA_BASE_URL": "http://remote.example.com/v1", "LMSTUDIO_BASE_URL": "http://remote.example.com/v1"}
	if got := provider.DiscoverLocal(context.Background(), src, &http.Client{Transport: rt}); len(got) != 0 || rt.calls != 0 {
		t.Fatalf("unsafe base URL was probed: models=%+v calls=%d", got, rt.calls)
	}

	// The same client does reach a safe address, so the zero count above is the
	// guard working and not a transport that never runs.
	if provider.DiscoverLocal(context.Background(), mapSource{}, &http.Client{Transport: rt}); rt.calls == 0 {
		t.Fatal("the default loopback address should be probed")
	}
}
