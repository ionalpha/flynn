package usage_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/anthropic"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/usage"
)

// cannedTransport answers every request with one recorded response body, so the adapter
// under test decodes real provider JSON and no network is involved.
type cannedTransport string

func (c cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(c))),
		Header:     make(http.Header),
	}, nil
}

// adapters maps each package under llm/ that implements [llm.Model] to a constructor
// that points it at a canned response. A new adapter package fails
// TestEveryAdapterHasAFixture until it is added here and given a fixture below.
var adapters = map[string]func(body string) llm.Model{
	"anthropic": func(body string) llm.Model {
		return anthropic.New(secret.New("k"), anthropic.WithHTTPClient(&http.Client{Transport: cannedTransport(body)}))
	},
	"openai": func(body string) llm.Model {
		return openai.New(secret.New("k"), openai.WithHTTPClient(&http.Client{Transport: cannedTransport(body)}))
	},
}

// fixture is one provider's response to a call with known cache traffic, and the exact
// llm.Usage, token total and cost the call must come out as. Every provider that
// provider.Providers names has one, because each provider reports its cache counters
// in its own shape and its adapter is the only place that shape is read.
type fixture struct {
	adapter string
	// body is the response in the shape the provider documents, with the figures chosen so
	// that adding a counter twice or dropping one changes the result.
	body string
	want llm.Usage
	// total is [usage.Tokens.Total]: input plus cache writes plus output.
	total int64
	// cost is the amount at the rates in fixtureTable.
	cost string
}

var fixtures = map[string]fixture{
	// 1000 uncached input, 500 read from cache, 200 written to it, 300 output. The
	// provider's input_tokens excludes both cache counters.
	"anthropic": {
		adapter: "anthropic",
		body: `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1000,"cache_creation_input_tokens":200,"cache_read_input_tokens":500,"output_tokens":300}}`,
		want:  llm.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 500, CacheWriteTokens: 200},
		total: 2000,
		// 1000*3 + 300*15 + 500*0.30 + 200*3.75 per million.
		cost: "0.0084",
	},
	// prompt_tokens includes the cached part, which is reported inside
	// prompt_tokens_details. The provider has no cache-write counter.
	"openai": {
		adapter: "openai",
		body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1500,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":500}}}`,
		want:  llm.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 500},
		total: 1800,
		// 1000*3 + 300*15 + 500*0.30 per million.
		cost: "0.00765",
	},
	// The hit and miss counters are flat fields and prompt_tokens is their sum.
	"deepseek": {
		adapter: "openai",
		body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1500,"completion_tokens":300,"prompt_cache_hit_tokens":500,"prompt_cache_miss_tokens":1000}}`,
		want:  llm.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 500},
		total: 1800,
		cost:  "0.00765",
	},
	// Gemini's OpenAI-compatible endpoint answers in the OpenAI shape.
	"gemini": {
		adapter: "openai",
		body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1500,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":500}}}`,
		want:  llm.Usage{InputTokens: 1500, OutputTokens: 300, CacheReadTokens: 500},
		total: 1800,
		cost:  "0.00765",
	},
	// A local server reports no cache counters; the whole prompt is uncached input.
	"llamacpp": localFixture(),
	"ollama":   localFixture(),
	"lmstudio": localFixture(),
}

func localFixture() fixture {
	return fixture{
		adapter: "openai",
		body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1500,"completion_tokens":300,"total_tokens":1800}}`,
		want:  llm.Usage{InputTokens: 1500, OutputTokens: 300},
		total: 1800,
		// 1500*3 + 300*15 per million.
		cost: "0.009",
	}
}

// fixtureTable gives every fixture provider the same rates, so a cost differs between
// fixtures only through the token split. Providers with no cache-write counter state no
// cache-write rate, which makes a stray write on one of them an unpriced call.
func fixtureTable(t *testing.T) *usage.Table {
	t.Helper()
	var rows []usage.Price
	for name := range fixtures {
		p := usage.Price{
			Provider: name, Model: "m", Effective: day("2026-10-01"), Currency: "USD",
			Input: nanos(t, "3"), Output: nanos(t, "15"), CacheRead: ptr(nanos(t, "0.30")),
			Source: "test fixture",
		}
		if name == "anthropic" {
			p.CacheWrite = ptr(nanos(t, "3.75"))
		}
		rows = append(rows, p)
	}
	tb, err := usage.NewTable(rows...)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tb
}

// TestAdapterUsageThroughPricing runs each provider's recorded response through its
// adapter, [usage.FromLLM] and [usage.Table.Price]. It pins the contract between the
// two packages: cache reads are inside Input, cache writes are on top of it, and a cache
// write is priced once, at the write rate.
func TestAdapterUsageThroughPricing(t *testing.T) {
	tb := fixtureTable(t)
	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			mk, ok := adapters[f.adapter]
			if !ok {
				t.Fatalf("fixture %s names adapter %q, which the test cannot construct", name, f.adapter)
			}
			resp, err := mk(f.body).Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "x")}})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if resp.Usage != f.want {
				t.Fatalf("adapter reported %+v, want %+v", resp.Usage, f.want)
			}
			tk := usage.FromLLM(resp.Usage)
			if got := tk.Total(); got != f.total {
				t.Fatalf("Total = %d, want %d", got, f.total)
			}
			rec := usage.Record{Run: "r", Call: "c", Provider: name, Model: "m", At: day("2026-11-01"), Tokens: tk}
			cost := tb.Price(rec)
			if !cost.Known {
				t.Fatalf("call unpriced: %s", cost.Reason)
			}
			if want := nanos(t, f.cost); cost.Amount != want {
				t.Fatalf("cost = %s, want %s", cost.Amount, want)
			}
		})
	}
}

// TestEveryAdapterHasAFixture fails when an llm.Model implementation exists with no
// fixture, or a provider is offered with none. The adapters are found by reading the
// source tree, so adding a package under llm/ with a Generate method needs no edit to
// this test to be noticed.
func TestEveryAdapterHasAFixture(t *testing.T) {
	used := map[string]bool{}
	for name, f := range fixtures {
		used[f.adapter] = true
		if _, ok := adapters[f.adapter]; !ok {
			t.Errorf("fixture %s uses adapter %q with no constructor in adapters", name, f.adapter)
		}
	}
	for _, dir := range adapterPackages(t) {
		if !used[dir] {
			t.Errorf("llm/%s implements llm.Model and no fixture runs through it; add one to fixtures", dir)
		}
		if _, ok := adapters[dir]; !ok {
			t.Errorf("llm/%s implements llm.Model and has no constructor in adapters", dir)
		}
	}
	for _, name := range provider.Providers() {
		if _, ok := fixtures[name]; !ok {
			t.Errorf("provider %q has no fixture; its usage shape is untested against the pricing contract", name)
		}
	}
	for name := range fixtures {
		known := false
		for _, p := range provider.Providers() {
			known = known || p == name
		}
		if !known {
			t.Errorf("fixture %q names no provider that provider.Providers returns", name)
		}
	}
}

// adapterPackages lists the directories directly under llm/ whose non-test sources
// declare a Generate method, the one method of [llm.Model], leaving out the test double.
func adapterPackages(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "llm")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var found []string
	for _, e := range entries {
		// llmtest is a scripted test double that reports no provider usage.
		if !e.IsDir() || e.Name() == "internal" || e.Name() == "llmtest" {
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, e.Name(), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if declaresGenerate(t, files) {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		t.Fatal("found no adapter under llm/; the scan is looking in the wrong place")
	}
	return found
}

func declaresGenerate(t *testing.T, files []string) bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "Generate" {
				return true
			}
		}
	}
	return false
}
