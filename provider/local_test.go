package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// wireRecorder is a chat server that records each request body and answers with a
// plain final message.
type wireRecorder struct {
	mu     sync.Mutex
	bodies []map[string]json.RawMessage
}

func (w *wireRecorder) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	w.mu.Lock()
	w.bodies = append(w.bodies, body)
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/json")
	_, _ = rw.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

func (w *wireRecorder) last(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.bodies) == 0 {
		t.Fatal("the server saw no request")
	}
	return w.bodies[len(w.bodies)-1]
}

var oneTool = llm.Tool{Name: "read_file", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}

func generate(t *testing.T, m llm.Model) {
	t.Helper()
	if _, err := m.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "hi")},
		Tools:    []llm.Tool{oneTool},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

// TestLocalKindsOfferTheSameToolsThroughTheirOwnField pins how each local provider
// hands the model its tools. llamacpp honors a decode-time grammar, so its tools ride
// the grammar field. Ollama and LM Studio ignore that field, so a grammar-only request
// would leave the model with no tools; they must send the tools field.
func TestLocalKindsOfferTheSameToolsThroughTheirOwnField(t *testing.T) {
	for _, tc := range []struct {
		spec        string
		wantGrammar bool
	}{
		{"llamacpp:m", true},
		{"ollama:m", false},
		{"lmstudio:m", false},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			rec := &wireRecorder{}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			name, _, _ := strings.Cut(tc.spec, ":")
			src := mapSource{strings.ToUpper(name) + "_BASE_URL": srv.URL + "/v1"}

			m, err := provider.ResolveWith(context.Background(), tc.spec, src)
			if err != nil {
				t.Fatalf("ResolveWith: %v", err)
			}
			generate(t, m)

			body := rec.last(t)
			_, hasGrammar := body["grammar"]
			tools := strings.TrimSpace(string(body["tools"]))
			hasTools := tools != "" && tools != "null" && tools != "[]"
			if hasGrammar != tc.wantGrammar {
				t.Fatalf("grammar field present = %v, want %v", hasGrammar, tc.wantGrammar)
			}
			if hasTools == tc.wantGrammar {
				t.Fatalf("tools field present = %v, want %v (the tools travel in exactly one field)", hasTools, !tc.wantGrammar)
			}
		})
	}
}

func TestOllamaAndLMStudioAreKeylessLoopbackByDefault(t *testing.T) {
	for _, name := range []string{"ollama", "lmstudio"} {
		t.Run(name, func(t *testing.T) {
			if _, ok := provider.KeyRef(name); ok {
				t.Fatalf("%s must be keyless", name)
			}
			if _, err := provider.ResolveWith(context.Background(), name+":m", mapSource{}); err != nil {
				t.Fatalf("keyless default should resolve: %v", err)
			}
			base := strings.ToUpper(name) + "_BASE_URL"
			if _, err := provider.ResolveWith(context.Background(), name+":m", mapSource{base: "http://gpu-box.example.com/v1"}); err == nil {
				t.Fatal("plaintext remote base URL must be refused")
			}
			if _, err := provider.ResolveWith(context.Background(), name+":m", mapSource{base: "http://127.0.0.1:9/v1"}); err != nil {
				t.Fatalf("loopback override should be allowed: %v", err)
			}
		})
	}
}

func writeProviders(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, provider.ProvidersFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadEndpoints(t *testing.T) {
	t.Run("missing file is no endpoints", func(t *testing.T) {
		eps, err := provider.LoadEndpoints(t.TempDir())
		if err != nil || len(eps.Providers) != 0 {
			t.Fatalf("eps=%v err=%v", eps, err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		dir := writeProviders(t, `{"allow_hosts":["gpu-box"],"providers":{"box":{"kind":"ollama","base_url":"http://gpu-box:11434/v1"},"gw":{"kind":"openai","base_url":"https://gw.example.com/v1","key":"GW_KEY","vision":true}}}`)
		eps, err := provider.LoadEndpoints(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(eps.Providers) != 2 || eps.Providers["gw"].Key != "GW_KEY" || !eps.Providers["gw"].Vision || eps.AllowHosts[0] != "gpu-box" {
			t.Fatalf("parsed wrongly: %+v", eps)
		}
	})
	for name, body := range map[string]string{
		"not json":         `nope`,
		"unknown field":    `{"providers":{},"extra":1}`,
		"shadows built-in": `{"providers":{"openai":{"kind":"openai","base_url":"https://x.example.com/v1"}}}`,
		"bad name":         `{"providers":{"Bad Name":{"kind":"openai","base_url":"https://x.example.com/v1"}}}`,
		"unknown kind":     `{"providers":{"x":{"kind":"vllm","base_url":"https://x.example.com/v1"}}}`,
		"no base url":      `{"providers":{"x":{"kind":"openai"}}}`,
		"not a url":        `{"providers":{"x":{"kind":"openai","base_url":"ftp://x"}}}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := provider.LoadEndpoints(writeProviders(t, body)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestLoadEndpointsReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, provider.ProvidersFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.LoadEndpoints(dir); err == nil {
		t.Fatal("a directory in place of the file should error")
	}
}

// withEndpoints returns a resolver over the providers file body, reading keys from src.
func withEndpoints(t *testing.T, body string, src mapSource) func(string) (llm.Model, error) {
	t.Helper()
	eps, err := provider.LoadEndpoints(writeProviders(t, body))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wrapped := provider.WithEndpoints(src, eps)
	return func(spec string) (llm.Model, error) { return provider.ResolveWith(ctx, spec, wrapped) }
}

func TestConfiguredEndpointKeylessHostPolicy(t *testing.T) {
	resolve := withEndpoints(t, `{"allow_hosts":["gpu-box"],"providers":{
		"here":{"kind":"ollama","base_url":"http://127.0.0.1:11434/v1"},
		"lan":{"kind":"ollama","base_url":"http://gpu-box:11434/v1"},
		"stranger":{"kind":"ollama","base_url":"http://other-box:11434/v1"},
		"tls":{"kind":"openai","base_url":"https://other-box/v1"}}}`, mapSource{})

	for _, ok := range []string{"here:m", "lan:m"} {
		if _, err := resolve(ok); err != nil {
			t.Errorf("%s should resolve: %v", ok, err)
		}
	}
	for _, refused := range []string{"stranger:m", "tls:m"} {
		_, err := resolve(refused)
		if err == nil {
			t.Fatalf("%s: a keyless endpoint on a host outside allow_hosts must be refused", refused)
		}
		if !strings.Contains(err.Error(), "allow_hosts") {
			t.Errorf("%s: the refusal should name the rule, got %v", refused, err)
		}
	}
}

func TestConfiguredEndpointWithKey(t *testing.T) {
	resolve := withEndpoints(t, `{"providers":{
		"gw":{"kind":"openai","base_url":"https://gw.example.com/v1","key":"GW_KEY"},
		"plain":{"kind":"openai","base_url":"http://gw.example.com/v1","key":"GW_KEY"}}}`, mapSource{"GW_KEY": "secret"})

	if m, err := resolve("gw:some-model"); err != nil || m == nil {
		t.Fatalf("keyed https endpoint: %v", err)
	}
	if m, err := withEndpoints(t, `{"providers":{"gw":{"kind":"openai","base_url":"https://gw.example.com/v1","key":"GW_KEY","vision":true}}}`,
		mapSource{"GW_KEY": "secret"})("gw:m"); err != nil || m == nil {
		t.Fatalf("keyed endpoint with vision: %v", err)
	}
	if _, err := resolve("plain:m"); err == nil {
		t.Fatal("a key must not travel over plaintext http to a remote host")
	}

	missing := withEndpoints(t, `{"providers":{"gw":{"kind":"openai","base_url":"https://gw.example.com/v1","key":"GW_KEY"}}}`, mapSource{})
	if _, err := missing("gw:m"); err == nil || !strings.Contains(err.Error(), "GW_KEY") {
		t.Fatalf("a missing key should name its reference, got %v", err)
	}
}

func TestConfiguredEndpointNamesAreListedAndUnknownStillErrors(t *testing.T) {
	eps, err := provider.LoadEndpoints(writeProviders(t, `{"providers":{"b":{"kind":"ollama","base_url":"http://localhost:1/v1"},"a":{"kind":"ollama","base_url":"http://localhost:2/v1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	src := provider.WithEndpoints(mapSource{}, eps)
	if got := strings.Join(provider.EndpointNames(src), ","); got != "a,b" {
		t.Fatalf("EndpointNames = %q", got)
	}
	if len(provider.EndpointNames(mapSource{})) != 0 {
		t.Fatal("a plain source carries no endpoints")
	}
	_, err = provider.ResolveWith(context.Background(), "nope:m", src)
	if err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("unknown provider error should list configured names, got %v", err)
	}
}

// TestConfiguredEndpointUsesItsKindsToolField checks that the kind, not the name, picks
// how tools travel: a configured llamacpp endpoint sends a grammar, a configured ollama
// or openai endpoint sends the tools field.
func TestConfiguredEndpointUsesItsKindsToolField(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		grammar bool
	}{{"llamacpp", true}, {"ollama", false}, {"openai", false}} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := &wireRecorder{}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			resolve := withEndpoints(t, `{"providers":{"mine":{"kind":"`+tc.kind+`","base_url":"`+srv.URL+`/v1"}}}`, mapSource{})
			m, err := resolve("mine:m")
			if err != nil {
				t.Fatal(err)
			}
			generate(t, m)
			if _, has := rec.last(t)["grammar"]; has != tc.grammar {
				t.Fatalf("grammar present = %v, want %v", has, tc.grammar)
			}
		})
	}
}

// TestSameServerUnderTwoProviderNamesGetsTheSameTools is the regression for a report
// that llamacpp: refused tools that openai: pointed at the same server was given. The
// grant a run holds is built from its toolset and never from the provider name; what
// differs by name is only how the tools travel, so the same set must reach the server
// under every name that reaches it.
func TestSameServerUnderTwoProviderNamesGetsTheSameTools(t *testing.T) {
	names := func(body map[string]json.RawMessage) string {
		if g, ok := body["grammar"]; ok {
			var s string
			_ = json.Unmarshal(g, &s)
			return s
		}
		var tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(body["tools"], &tools)
		var out []string
		for _, tl := range tools {
			out = append(out, tl.Function.Name)
		}
		return strings.Join(out, ",")
	}

	for _, spec := range []string{"llamacpp:m", "ollama:m", "lmstudio:m", "openai:m"} {
		t.Run(spec, func(t *testing.T) {
			rec := &wireRecorder{}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			src := mapSource{
				"LLAMACPP_BASE_URL": srv.URL + "/v1", "OLLAMA_BASE_URL": srv.URL + "/v1",
				"LMSTUDIO_BASE_URL": srv.URL + "/v1", "OPENAI_BASE_URL": srv.URL + "/v1", "OPENAI_API_KEY": "k",
			}
			m, err := provider.ResolveWith(context.Background(), spec, src)
			if err != nil {
				t.Fatal(err)
			}
			generate(t, m)
			if got := names(rec.last(t)); !strings.Contains(got, "read_file") {
				t.Fatalf("%s: the tool set did not reach the server: %q", spec, got)
			}
		})
	}
}

// TestBaseURLEnvVarsAreTheOnesResolveReads pins the list help and docs print: each
// variable it names must change where its provider connects, so a variable cannot be
// documented that nothing reads, and a provider cannot gain one that help omits.
func TestBaseURLEnvVarsAreTheOnesResolveReads(t *testing.T) {
	vars := provider.BaseURLEnvVars()
	if len(vars) != len(provider.Providers()) {
		t.Fatalf("%d base URL variables for %d providers: %v", len(vars), len(provider.Providers()), vars)
	}
	for _, v := range vars {
		name := strings.ToLower(strings.TrimSuffix(v, "_BASE_URL"))
		src := mapSource{v: "http://remote.example.com/v1", strings.ToUpper(name) + "_API_KEY": "k"}
		if _, err := provider.ResolveWith(context.Background(), name+":m", src); err == nil {
			t.Errorf("%s does not move %s: a plaintext remote value was accepted", v, name)
		}
	}
	if len(provider.VisionEnvVars()) != 3 {
		t.Errorf("VisionEnvVars = %v", provider.VisionEnvVars())
	}
}

// brokenSource fails every lookup with an error that is not "not found".
type brokenSource struct{}

func (brokenSource) Lookup(context.Context, string) (secret.Text, error) {
	return secret.Text{}, errors.New("vault locked")
}

// TestConfiguredEndpointFailuresAreReported covers an endpoint that did not come
// through LoadEndpoints, as a host embedding the package may build one: a base URL
// that does not parse is refused, and a key store that fails is not mistaken for a
// missing key.
func TestConfiguredEndpointFailuresAreReported(t *testing.T) {
	ctx := context.Background()
	eps := provider.Endpoints{Providers: map[string]provider.Endpoint{
		"bad":   {Kind: "ollama", BaseURL: "http://[::1"},
		"keyed": {Kind: "openai", BaseURL: "https://gw.example.com/v1", Key: "GW_KEY"},
	}}
	if _, err := provider.ResolveWith(ctx, "bad:m", provider.WithEndpoints(mapSource{}, eps)); err == nil {
		t.Error("a base_url that does not parse must be refused")
	}
	_, err := provider.ResolveWith(ctx, "keyed:m", provider.WithEndpoints(brokenSource{}, eps))
	if err == nil || !strings.Contains(err.Error(), "vault locked") || errors.Is(err, provider.ErrCredentialNotSet) {
		t.Errorf("a failing key store should surface its own error, got %v", err)
	}
}
