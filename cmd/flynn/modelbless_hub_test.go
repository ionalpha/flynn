package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/huggingface"
)

// blessRepo is the Hub state a bless test serves: the model card, the file tree, and the
// bytes of every small file the command downloads to hash.
type blessRepo struct {
	info    map[string]any
	tree    []map[string]any
	files   map[string][]byte
	infoErr int // when non-zero, the model card request answers with this status
	treeErr int // when non-zero, the tree request answers with this status
}

// blessLFS is a tree entry for a large, LFS-tracked file: the Hub records its content
// sha256 as the LFS object id, so bless pins the digest without downloading it.
func blessLFS(path string, size int64, sha string) map[string]any {
	return map[string]any{
		"type": "file", "path": path, "size": size,
		"lfs": map[string]any{"oid": sha, "size": size},
	}
}

// blessSmall is a tree entry for a small git-tracked file, which carries no content
// digest: bless has to fetch and hash it.
func blessSmall(path string, size int64) map[string]any {
	return map[string]any{"type": "file", "path": path, "size": size}
}

// newBlessHub serves a repo over TLS (the verified downloader refuses plaintext) and returns
// the Hub client and downloader wired to it, so bless runs end to end with no live network.
func newBlessHub(t *testing.T, repo blessRepo) (*huggingface.Client, *fetch.Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/tree/"):
			if repo.treeErr != 0 {
				w.WriteHeader(repo.treeErr)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(repo.tree)
		case strings.Contains(path, "/resolve/main/"):
			name := path[strings.Index(path, "/resolve/main/")+len("/resolve/main/"):]
			body, ok := repo.files[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		case strings.HasPrefix(path, "/api/models/"):
			if repo.infoErr != 0 {
				w.WriteHeader(repo.infoErr)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(repo.info)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	hub := huggingface.New(huggingface.WithHTTPClient(srv.Client()), huggingface.WithBaseURL(srv.URL))
	return hub, fetch.New(fetch.WithHTTPClient(srv.Client()))
}

// blessedSpec parses the catalog entry bless printed out of its output.
func blessedSpec(t *testing.T, out string) catalog.ModelSpec {
	t.Helper()
	i := strings.Index(out, "{")
	if i < 0 {
		t.Fatalf("no JSON entry printed:\n%s", out)
	}
	var spec catalog.ModelSpec
	if err := json.Unmarshal([]byte(out[i:]), &spec); err != nil {
		t.Fatalf("decode printed entry: %v\n%s", err, out[i:])
	}
	return spec
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestModelBlessPinsEveryFileToADigest is the trust anchor: a large file is pinned to the
// content digest the registry records, and a small file with no such id is downloaded once
// through the verified transport and pinned to the sha256 of the bytes that arrived.
func TestModelBlessPinsEveryFileToADigest(t *testing.T) {
	config := []byte(`{"max_position_embeddings":32768,"quantization_config":{"quant_method":"AWQ"}}`)
	tokenizer := []byte(`{"model":{"type":"BPE"}}`)
	weightsSHA := strings.Repeat("ab", 32)
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "Qwen/Qwen2.5-7B-Instruct-AWQ", "author": "Qwen", "cardData": map[string]any{"license": "apache-2.0"}},
		tree: []map[string]any{
			blessLFS("model.safetensors", 5_000_000_000, weightsSHA),
			blessSmall("config.json", int64(len(config))),
			blessSmall("tokenizer.json", int64(len(tokenizer))),
			blessSmall("README.md", 100),
		},
		files: map[string][]byte{"config.json": config, "tokenizer.json": tokenizer},
	})

	var out bytes.Buffer
	if err := modelBless(context.Background(), hub, dl, []string{"hf:Qwen/Qwen2.5-7B-Instruct-AWQ"}, &out); err != nil {
		t.Fatalf("modelBless: %v", err)
	}
	spec := blessedSpec(t, out.String())

	if spec.ID != "vllm:qwen2.5-7b-instruct-awq" {
		t.Errorf("id = %q, want the derived vllm id", spec.ID)
	}
	if spec.Kind != catalog.KindLocal || spec.Trust != catalog.TrustBlessed {
		t.Errorf("kind = %q trust = %q, want a blessed local entry", spec.Kind, spec.Trust)
	}
	if spec.License != "apache-2.0" || spec.Source.Publisher != "Qwen" || spec.Source.Registry != "huggingface" {
		t.Errorf("provenance not carried through: %+v / license %q", spec.Source, spec.License)
	}
	if spec.ContextTokens != 32768 || spec.ParamsB != 7 {
		t.Errorf("context = %d params = %v, want them read from the config and the name", spec.ContextTokens, spec.ParamsB)
	}
	if len(spec.Quants) != 1 {
		t.Fatalf("quants = %+v, want one", spec.Quants)
	}
	q := spec.Quants[0]
	if q.Name != "awq" || q.Format != catalog.FormatSafetensors {
		t.Errorf("quant = %q format = %q, want the config's scheme and safetensors", q.Name, q.Format)
	}
	want := map[string]string{
		"model.safetensors": "sha256:" + weightsSHA,
		"config.json":       "sha256:" + sha256Hex(config),
		"tokenizer.json":    "sha256:" + sha256Hex(tokenizer),
	}
	if len(q.Files) != len(want) {
		t.Fatalf("files = %+v, want only the serving files", q.Files)
	}
	for _, f := range q.Files {
		w, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected file in the manifest: %q", f.Name)
			continue
		}
		if f.Digest != w {
			t.Errorf("%s digest = %q, want %q", f.Name, f.Digest, w)
		}
		if f.Digest == "sha256:" || f.Digest == "" {
			t.Errorf("%s was blessed with no digest", f.Name)
		}
	}
}

// TestModelBlessRefusesUnsafeAndUnverifiableRepos covers the refusals: a pickle-only repo, a
// gated repo that cannot be fetched unattended, a repo with no weights at all, and a repo
// whose card declares no license. None of them may produce a catalog entry.
func TestModelBlessRefusesUnsafeAndUnverifiableRepos(t *testing.T) {
	cases := []struct {
		name string
		repo blessRepo
		want string
	}{
		{
			name: "pickle only",
			repo: blessRepo{
				info: map[string]any{"cardData": map[string]any{"license": "mit"}},
				tree: []map[string]any{
					blessLFS("pytorch_model.bin", 5_000_000_000, strings.Repeat("cd", 32)),
					blessSmall("config.json", 10),
				},
			},
			want: "code-executing weight files (pickle)",
		},
		{
			name: "no weights",
			repo: blessRepo{
				info: map[string]any{"cardData": map[string]any{"license": "mit"}},
				tree: []map[string]any{blessSmall("README.md", 10), blessSmall("config.json", 10)},
			},
			want: "no safetensors weights found",
		},
		{
			name: "gated",
			repo: blessRepo{
				info: map[string]any{"gated": "manual", "cardData": map[string]any{"license": "mit"}},
				tree: []map[string]any{blessLFS("model.safetensors", 10, strings.Repeat("ef", 32))},
			},
			want: "is gated",
		},
		{
			name: "no license",
			repo: blessRepo{
				info: map[string]any{"id": "acme/model-7B"},
				tree: []map[string]any{blessLFS("model.safetensors", 10, strings.Repeat("ef", 32))},
			},
			want: "declares no license",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub, dl := newBlessHub(t, c.repo)
			var out bytes.Buffer
			err := modelBless(context.Background(), hub, dl, []string{"hf:acme/model-7B"}, &out)
			if err == nil {
				t.Fatalf("expected a refusal, got an entry:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to name %q", err, c.want)
			}
			if strings.Contains(out.String(), "add this entry to the curated catalog") {
				t.Error("a refused repo must not print a catalog entry")
			}
		})
	}
}

// TestModelBlessOverridesAndBadRefs covers the flags a maintainer uses to fill a gap the Hub
// leaves (an undeclared license, a chosen id, quant, and chat template) and the argument
// errors that stop the command before it reaches the Hub.
func TestModelBlessOverridesAndBadRefs(t *testing.T) {
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "acme/tiny-1B"},
		tree: []map[string]any{blessLFS("model.safetensors", 2048, strings.Repeat("11", 32))},
	})

	var out bytes.Buffer
	args := []string{"--license", "mit", "--id", "vllm:tiny", "--quant", "fp8", "--chat-template", "llama3", "acme/tiny-1B"}
	if err := modelBless(context.Background(), hub, dl, args, &out); err != nil {
		t.Fatalf("modelBless: %v", err)
	}
	spec := blessedSpec(t, out.String())
	if spec.ID != "vllm:tiny" || spec.License != "mit" || spec.ChatTemplate != "llama3" {
		t.Errorf("overrides not applied: id=%q license=%q template=%q", spec.ID, spec.License, spec.ChatTemplate)
	}
	if spec.Quants[0].Name != "fp8" {
		t.Errorf("quant = %q, want the override", spec.Quants[0].Name)
	}
	// A repo with no config.json leaves the context unknown rather than guessed.
	if spec.ContextTokens != 0 {
		t.Errorf("context = %d, want it left unset when no config declares one", spec.ContextTokens)
	}

	for _, args := range [][]string{nil, {""}, {"not a hub ref"}} {
		var bad bytes.Buffer
		if err := modelBless(context.Background(), hub, dl, args, &bad); err == nil {
			t.Errorf("args %v: expected a refusal", args)
		}
	}
}

// TestModelBlessSurfacesHubFailures checks a Hub that is down or a repo that does not exist
// is reported as a bless failure naming the step that failed, not a half-written entry.
func TestModelBlessSurfacesHubFailures(t *testing.T) {
	cases := map[string]struct {
		repo blessRepo
		want string
	}{
		"card unreadable": {blessRepo{infoErr: http.StatusBadGateway}, "read model card"},
		"tree unreadable": {blessRepo{info: map[string]any{}, treeErr: http.StatusBadGateway}, "list files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hub, dl := newBlessHub(t, c.repo)
			var out bytes.Buffer
			err := modelBless(context.Background(), hub, dl, []string{"hf:acme/model-7B"}, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// TestModelBlessFailsWhenASmallFileCannotBeHashed checks the verified-download half: a file
// the Hub lists but does not serve cannot be pinned, so the entry is refused rather than
// blessed with a missing digest.
func TestModelBlessFailsWhenASmallFileCannotBeHashed(t *testing.T) {
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"cardData": map[string]any{"license": "mit"}},
		tree: []map[string]any{
			blessLFS("model.safetensors", 2048, strings.Repeat("22", 32)),
			blessSmall("tokenizer.json", 64), // listed, but the server has no bytes for it
		},
	})
	var out bytes.Buffer
	err := modelBless(context.Background(), hub, dl, []string{"hf:acme/model-7B"}, &out)
	if err == nil || !strings.Contains(err.Error(), "hash tokenizer.json") {
		t.Fatalf("error = %v, want the unhashable file named", err)
	}
}

// TestRunModelBlessValidatesBeforeReachingTheHub checks the command as wired: a missing
// reference is refused locally, so no request is made against the live Hub.
func TestRunModelBlessValidatesBeforeReachingTheHub(t *testing.T) {
	var out bytes.Buffer
	if err := runModelBless(nil, t.TempDir(), &out); err == nil {
		t.Fatal("expected a required-reference error")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed for a refused bless, got %q", out.String())
	}
}
