package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/internal/profilestore"
	"github.com/ionalpha/flynn/provider"
)

// TestProbeServerModelStoresProfileUnderServerSpec runs the wizard's adopted-model probe against
// a fake OpenAI-compatible server. It proves the model is reached through the server's own
// endpoint and that the measured profile is recorded under the provider:model spec, which is
// the key a later run of that spec reads its plan from.
func TestProbeServerModelStoresProfileUnderServerSpec(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		requests++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_BASE_URL", srv.URL+"/v1")

	dataDir := t.TempDir()
	lm := provider.LocalModel{Server: "ollama", ID: "wizard-test:7b", Params: "7B", Quant: "Q4_K_M", Tools: true}

	var out bytes.Buffer
	if err := probeServerModel(t.Context(), dataDir, lm, &out); err != nil {
		t.Fatalf("probeServerModel: %v\n%s", err, out.String())
	}
	if requests == 0 {
		t.Fatal("the fake server was never asked: the probe did not reach the model through the server")
	}
	if !strings.Contains(out.String(), "reliability for ollama:wizard-test:7b") {
		t.Fatalf("report does not name the server spec:\n%s", out.String())
	}

	store, err := openDataStore(t.Context(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := governed.Registry()
	if err != nil {
		t.Fatal(err)
	}
	src, err := profilestore.NewSource(t.Context(), store.Resources(reg))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Profile(lm.Spec()); !ok {
		t.Fatalf("no profile stored under %q", lm.Spec())
	}
	if _, ok := src.Profile(lm.ID); ok {
		t.Fatalf("a profile was stored under the bare model id %q", lm.ID)
	}
}
