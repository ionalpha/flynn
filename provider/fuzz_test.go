package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/provider"
)

// FuzzLoadEndpoints feeds arbitrary bytes to the providers file loader. A file the
// operator edits by hand is foreign input: it must be accepted or refused, never panic,
// and whatever is accepted must name only providers that resolve to a client or refuse
// with an error.
func FuzzLoadEndpoints(f *testing.F) {
	f.Add([]byte(`{"providers":{"box":{"kind":"ollama","base_url":"http://localhost:11434/v1"}}}`))
	f.Add([]byte(`{"allow_hosts":["gpu-box"],"providers":{}}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, raw []byte) {
		if err := os.WriteFile(filepath.Join(dir, provider.ProvidersFile), raw, 0o600); err != nil {
			t.Skip()
		}
		eps, err := provider.LoadEndpoints(dir)
		if err != nil {
			return
		}
		src := provider.WithEndpoints(mapSource{}, eps)
		for _, name := range provider.EndpointNames(src) {
			_, _ = provider.ResolveWith(context.Background(), name+":m", src)
		}
	})
}

// FuzzDiscoverLocal feeds arbitrary bytes to the decoders for what Ollama and LM Studio
// answer. A port that answers with anything must yield models or nothing, never a panic.
func FuzzDiscoverLocal(f *testing.F) {
	f.Add([]byte(`{"models":[{"name":"gpt-oss:20b","details":{"parameter_size":"20.9B"}}]}`))
	f.Add([]byte(`{"data":[{"id":"qwen3"}]}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	var body atomic.Value
	body.Store([]byte(nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body.Load().([]byte))
	}))
	f.Cleanup(srv.Close)
	src := mapSource{"OLLAMA_BASE_URL": srv.URL + "/v1", "LMSTUDIO_BASE_URL": srv.URL + "/v1"}
	f.Fuzz(func(t *testing.T, raw []byte) {
		body.Store(raw)
		for _, m := range provider.DiscoverLocal(context.Background(), src, nil) {
			if m.ID == "" {
				t.Fatalf("a model with no id was listed: %+v", m)
			}
		}
	})
}
