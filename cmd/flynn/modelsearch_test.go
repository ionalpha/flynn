package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/huggingface"
)

// stubHub serves a fixed model list over a local HTTP server and records the query the
// client sent, so the flags a search command parses are checked against the request that
// actually goes out.
type stubHub struct {
	client *huggingface.Client
	query  url.Values
}

func newStubHub(t *testing.T, entries []map[string]any) *stubHub {
	t.Helper()
	hub := &stubHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(srv.Close)
	hub.client = huggingface.New(huggingface.WithHTTPClient(srv.Client()), huggingface.WithBaseURL(srv.URL))
	return hub
}

// failingHub answers every request with a server error, standing in for a Hub that is down.
func failingHub(t *testing.T) *huggingface.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return huggingface.New(huggingface.WithHTTPClient(srv.Client()), huggingface.WithBaseURL(srv.URL))
}

func hubEntry(id string, downloads, likes int64, pipeline string, tags ...string) map[string]any {
	return map[string]any{
		"id":           id,
		"downloads":    downloads,
		"likes":        likes,
		"pipeline_tag": pipeline,
		"tags":         tags,
	}
}

func TestModelSearchRejectsAnEmptyQuery(t *testing.T) {
	var out bytes.Buffer
	err := modelSearch(context.Background(), newStubHub(t, nil).client, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "search term is required") {
		t.Fatalf("expected a required-term error, got %v", err)
	}
}

// TestRunModelSearchValidatesBeforeReachingTheHub checks the command as wired: a search
// with no term is refused locally, so no request is made against the live Hub.
func TestRunModelSearchValidatesBeforeReachingTheHub(t *testing.T) {
	var out bytes.Buffer
	if err := runModelSearch(nil, t.TempDir(), &out); err == nil {
		t.Fatal("expected an error for a search with no term")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed for a refused search, got %q", out.String())
	}
}

func TestModelSearchRejectsABadLimit(t *testing.T) {
	for _, limit := range []string{"0", "-3", "many"} {
		var out bytes.Buffer
		err := modelSearch(context.Background(), newStubHub(t, nil).client, []string{"--limit", limit, "qwen"}, &out)
		if err == nil || !strings.Contains(err.Error(), "--limit must be a positive number") {
			t.Fatalf("--limit %s: expected a positive-number error, got %v", limit, err)
		}
	}
}

// TestModelSearchPassesFlagsToTheHub locks the mapping from the command's flags onto the
// Hub query: the free text, the author, the sort, the limit, and the tag filters the
// convenience flags expand into.
func TestModelSearchPassesFlagsToTheHub(t *testing.T) {
	hub := newStubHub(t, []map[string]any{hubEntry("Qwen/Qwen2.5-7B", 10, 2, "text-generation", "gguf")})
	var out bytes.Buffer
	args := []string{
		"--author", "Qwen", "--sort", "likes", "--limit", "5",
		"--tag", "text-generation", "--gguf", "--safetensors",
		"qwen2.5", "7b",
	}
	if err := modelSearch(context.Background(), hub.client, args, &out); err != nil {
		t.Fatalf("modelSearch: %v", err)
	}
	if got := hub.query.Get("search"); got != "qwen2.5 7b" {
		t.Errorf("search text = %q, want the positional words joined", got)
	}
	if got := hub.query.Get("author"); got != "Qwen" {
		t.Errorf("author = %q", got)
	}
	if got := hub.query.Get("sort"); got != "likes" {
		t.Errorf("sort = %q, want likes", got)
	}
	if got := hub.query.Get("limit"); got != "5" {
		t.Errorf("limit = %q, want 5", got)
	}
	filters := hub.query["filter"]
	want := map[string]bool{"text-generation": true, "gguf": true, "safetensors": true}
	if len(filters) != len(want) {
		t.Fatalf("filters = %v, want the tag plus both format flags", filters)
	}
	for _, f := range filters {
		if !want[f] {
			t.Errorf("unexpected filter %q", f)
		}
	}
}

// TestModelSearchSearchesByAuthorAlone checks that an author or a tag is a search on its
// own: no free-text term is required when the query is already narrowed.
func TestModelSearchSearchesByAuthorAlone(t *testing.T) {
	hub := newStubHub(t, []map[string]any{hubEntry("Qwen/Qwen3-8B", 1000, 5, "text-generation", "safetensors")})
	var out bytes.Buffer
	if err := modelSearch(context.Background(), hub.client, []string{"--author", "Qwen"}, &out); err != nil {
		t.Fatalf("modelSearch: %v", err)
	}
	if !strings.Contains(out.String(), "hf:Qwen/Qwen3-8B") {
		t.Errorf("the candidate should be printed bless-ready, got:\n%s", out.String())
	}
}

// TestModelSearchHidesPickleOnlyByDefault is the security-relevant behavior: a repo with
// no safe weight format is filtered out unless --all is passed, and the count of what was
// hidden is reported rather than silently dropped.
func TestModelSearchHidesPickleOnlyByDefault(t *testing.T) {
	entries := []map[string]any{
		hubEntry("safe/one", 2_500_000, 1_200, "text-generation", "safetensors"),
		hubEntry("pickle/only", 900, 3, "text-generation", "pytorch"),
	}

	var out bytes.Buffer
	if err := modelSearch(context.Background(), newStubHub(t, entries).client, []string{"qwen"}, &out); err != nil {
		t.Fatalf("modelSearch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "hf:safe/one") {
		t.Errorf("the safe candidate must be shown, got:\n%s", got)
	}
	if strings.Contains(got, "pickle/only") {
		t.Errorf("a pickle-only candidate must be hidden by default, got:\n%s", got)
	}
	if !strings.Contains(got, "1 pickle-only match(es) hidden; --all to show") {
		t.Errorf("the hidden count must be reported, got:\n%s", got)
	}
	// The popularity signals are rendered compactly next to the candidate.
	if !strings.Contains(got, "2.5M downloads, 1.2k likes | text-generation | safetensors") {
		t.Errorf("signal line not rendered as expected, got:\n%s", got)
	}

	var all bytes.Buffer
	if err := modelSearch(context.Background(), newStubHub(t, entries).client, []string{"--all", "qwen"}, &all); err != nil {
		t.Fatalf("modelSearch --all: %v", err)
	}
	if !strings.Contains(all.String(), "hf:pickle/only") || !strings.Contains(all.String(), "no safe format (pickle-only)") {
		t.Errorf("--all must show the pickle-only candidate and name its format, got:\n%s", all.String())
	}
}

// TestModelSearchReportsEmptyOutcomes covers the two no-result endings: nothing matched at
// all, and everything that matched was filtered out as pickle-only.
func TestModelSearchReportsEmptyOutcomes(t *testing.T) {
	var none bytes.Buffer
	if err := modelSearch(context.Background(), newStubHub(t, nil).client, []string{"nothing"}, &none); err != nil {
		t.Fatalf("modelSearch: %v", err)
	}
	if !strings.Contains(none.String(), "no models matched.") {
		t.Errorf("want a no-match line, got %q", none.String())
	}

	onlyPickle := []map[string]any{hubEntry("pickle/only", 1, 0, "", "pytorch")}
	var filtered bytes.Buffer
	if err := modelSearch(context.Background(), newStubHub(t, onlyPickle).client, []string{"x"}, &filtered); err != nil {
		t.Fatalf("modelSearch: %v", err)
	}
	if !strings.Contains(filtered.String(), "no candidates with a safe weight format; 1 match(es) are pickle-only") {
		t.Errorf("want the all-filtered explanation, got %q", filtered.String())
	}
	if strings.Contains(filtered.String(), "bless a candidate") {
		t.Error("with nothing shown there is nothing to bless, so the hint must be omitted")
	}
}

func TestModelSearchWrapsAHubFailure(t *testing.T) {
	var out bytes.Buffer
	err := modelSearch(context.Background(), failingHub(t), []string{"qwen"}, &out)
	if err == nil || !strings.HasPrefix(err.Error(), "models search: ") {
		t.Fatalf("a Hub failure must surface as a models search error, got %v", err)
	}
}

func TestPrintSearchResultNamesBothSafeFormats(t *testing.T) {
	cases := []struct {
		name string
		r    huggingface.SearchResult
		want string
	}{
		{"both", huggingface.SearchResult{ID: "a/b", Tags: []string{"GGUF", "safetensors"}}, "safetensors + gguf"},
		{"gguf", huggingface.SearchResult{ID: "a/b", Tags: []string{"gguf"}}, "gguf"},
		{"safetensors", huggingface.SearchResult{ID: "a/b", Tags: []string{"safetensors"}}, "safetensors"},
		{"neither", huggingface.SearchResult{ID: "a/b", Tags: []string{"pytorch"}}, "no safe format (pickle-only)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			printSearchResult(&out, c.r)
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("format label = %q, want it to contain %q", out.String(), c.want)
			}
			// A result with no pipeline tag still says so rather than printing a blank.
			if !strings.Contains(out.String(), "unspecified") {
				t.Errorf("an absent pipeline must render as unspecified, got %q", out.String())
			}
		})
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		999:       "999",
		1_000:     "1.0k",
		1_234:     "1.2k",
		999_999:   "1000.0k",
		1_000_000: "1.0M",
		3_450_000: "3.5M",
	}
	for n, want := range cases {
		if got := humanCount(n); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTakeValues(t *testing.T) {
	rest, values := takeValues([]string{"a", "--tag", "gguf", "b", "--tag", "text-generation"}, "--tag")
	if len(values) != 2 || values[0] != "gguf" || values[1] != "text-generation" {
		t.Errorf("values = %v, want both tags collected in order", values)
	}
	if len(rest) != 2 || rest[0] != "a" || rest[1] != "b" {
		t.Errorf("rest = %v, want the positional words only", rest)
	}
	// A trailing flag with no value is left in place rather than eating past the end.
	rest, values = takeValues([]string{"a", "--tag"}, "--tag")
	if len(values) != 0 {
		t.Errorf("a valueless trailing flag must collect nothing, got %v", values)
	}
	if len(rest) != 2 {
		t.Errorf("rest = %v, want the dangling flag preserved", rest)
	}
}
