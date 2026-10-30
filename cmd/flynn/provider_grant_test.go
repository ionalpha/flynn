package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// toolServer is a chat server that behaves like the real ones on the wire: it calls a
// tool once, in whichever form the request asked for (a grammar-constrained reply for a
// request carrying a grammar, a tool_calls reply otherwise), then finishes. It keeps
// every request body so a test can read what the model was told about the tool's result.
type toolServer struct {
	mu     sync.Mutex
	bodies []string
}

func (s *toolServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	first := len(s.bodies) == 1
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case !first:
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`))
	case strings.Contains(body, `"grammar"`):
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"glob\",\"arguments\":{\"pattern\":\"*\"}}"},"finish_reason":"stop"}],"usage":{}}`))
	default:
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*\"}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
	}
}

func (s *toolServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// TestTheSameServerUnderAnyProviderNameRunsTheSameTool is the regression for a report
// that a goal on llamacpp: ended in capability_denied where openai: pointed at the same
// server ran its tools. It drives a whole goal through the real assembly, once per
// provider name, against one fake server, and requires that the tool the model called
// ran in every case: the grant comes from the run's toolset, not from the name.
func TestTheSameServerUnderAnyProviderNameRunsTheSameTool(t *testing.T) {
	for _, spec := range []string{"openai:m", "llamacpp:m", "ollama:m", "lmstudio:m"} {
		t.Run(spec, func(t *testing.T) {
			srv := &toolServer{}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			for _, v := range provider.BaseURLEnvVars() {
				t.Setenv(v, "")
			}
			t.Setenv("OPENAI_API_KEY", "k")
			t.Setenv("OPENAI_BASE_URL", ts.URL+"/v1")
			t.Setenv("LLAMACPP_BASE_URL", ts.URL+"/v1")
			t.Setenv("OLLAMA_BASE_URL", ts.URL+"/v1")
			t.Setenv("LMSTUDIO_BASE_URL", ts.URL+"/v1")

			model, err := provider.ResolveWith(context.Background(), spec, secret.EnvSource{})
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			store := memStore(t)
			if _, _, _, err := (driveRequest{
				out:       io.Discard,
				model:     model,
				plan:      harness.Plan{},
				workdir:   t.TempDir(),
				objective: "list the files",
				system:    defaultSystemPrompt,
				rstore:    store.Resources(mustRegistry(t)),
				jq:        store.Jobs(),
				log:       store.Log(),
			}).execute(ctx); err != nil {
				t.Fatalf("drive: %v", err)
			}

			bodies := srv.seen()
			if len(bodies) < 2 {
				t.Fatalf("the model's tool call never reached a second turn (%d requests)", len(bodies))
			}
			for i, b := range bodies {
				if strings.Contains(b, "capability_denied") {
					t.Fatalf("request %d reports a denied capability under %s:\n%s", i+1, spec, b)
				}
			}
		})
	}
}
