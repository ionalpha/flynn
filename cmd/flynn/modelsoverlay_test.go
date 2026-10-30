package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/huggingface"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/sandbox"
)

// overlayDir points the catalog at a fresh data directory for one test and restores the
// previous one afterwards.
func overlayDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := catalogDataDir
	setCatalogDataDir(dir)
	t.Cleanup(func() { setCatalogDataDir(prev) })
	return dir
}

// embeddedLocalID is the id of a local model the embedded catalog ships.
func embeddedLocalID(t *testing.T) string {
	t.Helper()
	base, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range base.Models {
		if m.Local() {
			return m.ID
		}
	}
	t.Fatal("the embedded catalog has no local model")
	return ""
}

// writeGGUF writes a small file with a .gguf name and returns its path and sha256.
func writeGGUF(t *testing.T, name string) (path, sum string) {
	t.Helper()
	body := append([]byte("GGUF"), bytes.Repeat([]byte{7}, 4096)...)
	path = filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	return path, hex.EncodeToString(h[:])
}

func userEntry(t *testing.T, id string) catalog.ModelSpec {
	t.Helper()
	m, ok := userModelEntry(id)
	if !ok {
		t.Fatalf("%q is not a user entry in the loaded catalog", id)
	}
	return m
}

func TestAddLocalFileRecordsAnUnverifiedEntry(t *testing.T) {
	dir := overlayDir(t)
	src, sum := writeGGUF(t, "My Tiny-7B-Q4_K_M.gguf")

	var out bytes.Buffer
	if err := runModelAdd([]string{"--yes", "--license", "mit", src}, dir, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	m := userEntry(t, "local:my-tiny-7b-q4_k_m")
	if m.Trust != catalog.TrustLocal || !m.Unverified() {
		t.Errorf("trust = %q, want a local, unverified entry", m.Trust)
	}
	q := m.Quants[0]
	if q.Name != "Q4_K_M" || q.Digest != "sha256:"+sum || m.License != "mit" {
		t.Errorf("entry = %+v, want the file's own quant, digest and the given license", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "models", weightsFileName(m.ID, q))); err != nil {
		t.Errorf("the weights were not copied where the host looks for them: %v", err)
	}
	if !isLocalModelID(m.ID) {
		t.Error("an added model is not selectable by id")
	}
	if isBlessedModelID(m.ID) {
		t.Error("an added model satisfied the embedded-catalog test")
	}

	var list bytes.Buffer
	if err := runModels(nil, dir, &list); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(list.String(), "\n") {
		if strings.HasPrefix(line, m.ID) && !strings.Contains(line, "unverified") {
			t.Errorf("listing row %q does not say unverified", line)
		}
	}
	if !strings.Contains(list.String(), m.ID) {
		t.Errorf("the listing does not show %s:\n%s", m.ID, list.String())
	}
}

func TestAddRefusesAnEmbeddedID(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")
	id := embeddedLocalID(t)

	err := runModelAdd([]string{"--yes", "--id", id, src}, dir, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("err = %v, want a refusal naming %q", err, id)
	}
	if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
		t.Error("a refused add left an overlay file behind")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "models")); len(entries) != 0 {
		t.Error("a refused add copied the weights before refusing")
	}
}

func TestAddWithoutConsentWritesNothing(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")

	// No --yes: the session is not an interactive confirmation, so the add must not go through.
	err := runModelAdd([]string{src}, dir, &bytes.Buffer{})
	if err == nil {
		t.Fatal("an add without consent succeeded")
	}
	if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
		t.Error("an unconsented add left an overlay file behind")
	}
}

func TestAddRejectsBadArguments(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")
	notGGUF := filepath.Join(t.TempDir(), "w.bin")
	if err := os.WriteFile(notGGUF, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no source":         {},
		"two sources":       {src, src},
		"colon in id":       {"--id", "ollama:mine", src},
		"unknown format":    {notGGUF},
		"missing file":      {filepath.Join(dir, "absent.gguf")},
		"bad ollama ref":    {"ollama:../x"},
		"not a hub ref":     {"https://huggingface.co/"},
		"bare name no gguf": {"qwen2.5:7b"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runModelAdd(append([]string{"--yes"}, args...), dir, &bytes.Buffer{}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRemoveDeletesTheEntryAndItsWeights(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")
	if err := runModelAdd([]string{"--yes", src}, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	m := userEntry(t, "local:tiny")
	weights := filepath.Join(dir, "models", weightsFileName(m.ID, m.Quants[0]))

	var out bytes.Buffer
	if err := runModelRemove([]string{m.ID}, dir, &out); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := userModelEntry(m.ID); ok {
		t.Error("the entry survived its removal")
	}
	if _, err := os.Stat(weights); err == nil {
		t.Error("the copied weights survived the removal")
	}
	if !strings.Contains(out.String(), "1 downloaded file") {
		t.Errorf("output = %q, want the deleted file count", out.String())
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("remove deleted the user's original file: %v", err)
	}
}

func TestRemoveRefusesACatalogEntry(t *testing.T) {
	dir := overlayDir(t)
	id := embeddedLocalID(t)
	err := runModelRemove([]string{id}, dir, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "catalog entry") {
		t.Fatalf("err = %v, want a refusal naming %q as a catalog entry", err, id)
	}
	if err := runModelRemove([]string{"local:never-added"}, dir, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not one of your entries") {
		t.Errorf("err = %v, want a refusal for an id that was never added", err)
	}
	if err := runModelRemove(nil, dir, &bytes.Buffer{}); err == nil {
		t.Error("remove with no id succeeded")
	}
}

func TestAddHubPinsTheHubDigest(t *testing.T) {
	dir := overlayDir(t)
	sha := strings.Repeat("ab", 32)
	hub, _ := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "acme/Tiny-1B-GGUF", "cardData": map[string]any{"license": "apache-2.0"}},
		tree: []map[string]any{
			blessLFS("tiny-1b-Q8_0.gguf", 2000, strings.Repeat("cd", 32)),
			blessLFS("tiny-1b-Q4_K_M.gguf", 1000, sha),
			blessLFS("mmproj-tiny.gguf", 10, strings.Repeat("ee", 32)),
			blessLFS("tiny-1b-Q2_K-00001-of-00002.gguf", 5, strings.Repeat("ff", 32)),
		},
	})

	var out bytes.Buffer
	if err := addHub(context.Background(), hub, dir, []string{"hf:acme/Tiny-1B-GGUF"}, true, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	m := userEntry(t, "hub:tiny-1b-gguf")
	q := m.Quants[0]
	if q.Name != "Q4_K_M" || q.Digest != "sha256:"+sha || q.SizeBytes != 1000 {
		t.Errorf("quant = %+v, want the Q4_K_M file with the Hub's digest", q)
	}
	if m.License != "apache-2.0" || m.Source.Publisher != "acme" || m.Unverified() == false {
		t.Errorf("entry = %+v", m)
	}
}

func TestAddHubRefusals(t *testing.T) {
	cases := map[string]struct {
		repo blessRepo
		args []string
		want string
	}{
		"gated": {
			repo: blessRepo{info: map[string]any{"gated": "manual"}, tree: []map[string]any{blessLFS("a-Q4_K_M.gguf", 1, strings.Repeat("ab", 32))}},
			args: []string{"hf:acme/m"},
			want: "gated",
		},
		"no gguf": {
			repo: blessRepo{info: map[string]any{}, tree: []map[string]any{blessLFS("model.safetensors", 1, strings.Repeat("ab", 32))}},
			args: []string{"hf:acme/m"},
			want: "no single-file GGUF",
		},
		"no content digest": {
			repo: blessRepo{info: map[string]any{}, tree: []map[string]any{blessSmall("tiny-Q4_K_M.gguf", 100)}},
			args: []string{"hf:acme/m"},
			want: "no content digest",
		},
		"no such quant": {
			repo: blessRepo{info: map[string]any{}, tree: []map[string]any{blessLFS("a-Q4_K_M.gguf", 1, strings.Repeat("ab", 32))}},
			args: []string{"hf:acme/m", "--quant", "Q8_0"},
			want: "no single-file GGUF",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := overlayDir(t)
			hub, _ := newBlessHub(t, c.repo)
			err := addHub(context.Background(), hub, dir, c.args, true, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
				t.Error("a refused add wrote the overlay")
			}
		})
	}
}

func TestSelectGGUF(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	f := func(path string, size int64, sum string) huggingface.File {
		return huggingface.File{Path: path, Size: size, SHA256: sum}
	}
	files := []huggingface.File{
		f("m-Q8_0.gguf", 800, sha),
		f("m-Q4_K_M.gguf", 400, sha),
		f("m-Q3_K_S.gguf", 300, sha),
		f("sub/m-IQ2_XS.gguf", 200, sha),
		f("mmproj-m.gguf", 50, sha),
		f("m-Q6_K-00001-of-00002.gguf", 100, sha),
		f("README.md", 1, ""),
	}
	cases := []struct {
		name, file, quant, want string
	}{
		{name: "prefers Q4_K_M", want: "m-Q4_K_M.gguf"},
		{name: "by quant", quant: "q8_0", want: "m-Q8_0.gguf"},
		{name: "by file base name", file: "m-Q3_K_S.gguf", want: "m-Q3_K_S.gguf"},
		{name: "by path", file: "sub/m-IQ2_XS.gguf", want: "sub/m-IQ2_XS.gguf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectGGUF(files, c.file, c.quant)
			if err != nil || got.Path != c.want {
				t.Fatalf("selectGGUF = %q, %v; want %q", got.Path, err, c.want)
			}
		})
	}
	if _, err := selectGGUF(files, "mmproj-m.gguf", ""); err == nil {
		t.Error("a projector file was selectable")
	}
	if _, err := selectGGUF(files, "m-Q6_K-00001-of-00002.gguf", ""); err == nil {
		t.Error("one shard of a split model was selectable")
	}
	// Without a Q4_K_M the smallest file wins.
	got, err := selectGGUF([]huggingface.File{f("a-Q8_0.gguf", 8, sha), f("a-Q5_K_M.gguf", 5, sha)}, "", "")
	if err != nil || got.Path != "a-Q5_K_M.gguf" {
		t.Errorf("selectGGUF = %q, %v; want the smallest", got.Path, err)
	}
}

func TestParseOllamaRef(t *testing.T) {
	cases := []struct {
		ref, ns, name, tag string
		ok                 bool
	}{
		{"ollama:llama3", "library", "llama3", "latest", true},
		{"ollama:llama3:8b", "library", "llama3", "8b", true},
		{"ollama:someone/model:q4", "someone", "model", "q4", true},
		{"ollama:", "", "", "", false},
		{"ollama:../x", "", "", "", false},
		{"ollama:a/b/c", "", "", "", false},
		{"ollama:Upper", "", "", "", false},
		{"ollama:name:ta g", "", "", "", false},
		{"ollama:name?x=1", "", "", "", false},
	}
	for _, c := range cases {
		ns, name, tag, err := parseOllamaRef(c.ref)
		if (err == nil) != c.ok || ns != c.ns || name != c.name || tag != c.tag {
			t.Errorf("parseOllamaRef(%q) = %q %q %q, %v", c.ref, ns, name, tag, err)
		}
	}
}

// ollamaStub serves one manifest over TLS and points the add at it.
func ollamaStub(t *testing.T, manifest any, status int) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/tiny/manifests/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(srv.Close)
	prevReg, prevClient := ollamaRegistry, ollamaHTTPClient
	ollamaRegistry = srv.URL
	ollamaHTTPClient = func() *http.Client { return srv.Client() }
	t.Cleanup(func() { ollamaRegistry, ollamaHTTPClient = prevReg, prevClient })
}

func ollamaManifestWith(mediaType, digest string, size int64) map[string]any {
	return map[string]any{"layers": []map[string]any{
		{"mediaType": "application/vnd.ollama.image.template", "digest": "sha256:" + strings.Repeat("00", 32), "size": 10},
		{"mediaType": mediaType, "digest": digest, "size": size},
	}}
}

func TestAddOllamaPinsTheManifestDigest(t *testing.T) {
	dir := overlayDir(t)
	sum := strings.Repeat("9f", 32)
	ollamaStub(t, ollamaManifestWith("application/vnd.ollama.image.model", "sha256:"+sum, 4096), http.StatusOK)

	var out bytes.Buffer
	if err := runModelAdd([]string{"--yes", "ollama:tiny"}, dir, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	id := "ollama.com/library/tiny:latest"
	m := userEntry(t, id)
	q := m.Quants[0]
	if q.Digest != "sha256:"+sum || q.SizeBytes != 4096 || !strings.HasSuffix(q.URL, "/v2/library/tiny/blobs/sha256:"+sum) {
		t.Errorf("quant = %+v, want the manifest's blob and digest", q)
	}
	if m.Source.Registry != "ollama" || !m.Unverified() {
		t.Errorf("entry = %+v", m)
	}
	// The id must stay out of the provider:model namespace, or selecting it would reroute a
	// spec that names a model on a running Ollama server.
	if strings.HasPrefix(id, "ollama:") {
		t.Errorf("id %q collides with the ollama provider spec", id)
	}
}

func TestAddOllamaRefusals(t *testing.T) {
	good := strings.Repeat("9f", 32)
	cases := map[string]struct {
		manifest any
		status   int
	}{
		"not found":      {map[string]any{}, http.StatusNotFound},
		"no model layer": {ollamaManifestWith("application/vnd.ollama.image.params", "sha256:"+good, 10), http.StatusOK},
		"short digest":   {ollamaManifestWith("application/vnd.ollama.image.model", "sha256:abc", 10), http.StatusOK},
		"non hex digest": {ollamaManifestWith("application/vnd.ollama.image.model", "sha256:"+strings.Repeat("zz", 32), 10), http.StatusOK},
		"zero size":      {ollamaManifestWith("application/vnd.ollama.image.model", "sha256:"+good, 0), http.StatusOK},
		"wrong algo":     {ollamaManifestWith("application/vnd.ollama.image.model", "md5:"+good, 10), http.StatusOK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := overlayDir(t)
			ollamaStub(t, c.manifest, c.status)
			if err := runModelAdd([]string{"--yes", "ollama:tiny"}, dir, &bytes.Buffer{}); err == nil {
				t.Fatal("expected a refusal")
			}
			if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
				t.Error("a refused add wrote the overlay")
			}
		})
	}
}

// TestUserEntrySourceIsNeverACatalogSource covers the trust decision: whatever an entry
// claims, it is classified by where its bytes come from and never as the trusted catalog.
func TestUserEntrySourceIsNeverACatalogSource(t *testing.T) {
	known := func(owner string) bool { return modelsource.KnownPublisher(owner, catalogPublishers()...) }
	pub := "https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-GGUF"
	cases := []struct {
		name string
		m    catalog.ModelSpec
		kind modelsource.Kind
	}{
		{
			name: "local file",
			m: catalog.ModelSpec{
				ID: "local:x", Source: catalog.Source{Registry: localRegistry, URL: "file:///tmp/x.gguf"},
				Quants: []catalog.Quant{{URL: "file:///tmp/x.gguf"}},
			},
			kind: modelsource.KindFile,
		},
		{
			name: "hub, fetched from the repository it names",
			m: catalog.ModelSpec{
				ID: "hub:x", Source: catalog.Source{Registry: "huggingface", URL: "https://huggingface.co/rando/x"},
				Quants: []catalog.Quant{{URL: "https://huggingface.co/rando/x/resolve/main/x.gguf"}},
			},
			kind: modelsource.KindHuggingFace,
		},
		{
			name: "claims a known publisher but fetches elsewhere",
			m: catalog.ModelSpec{
				ID: "hub:y", Source: catalog.Source{Registry: "huggingface", URL: pub},
				Quants: []catalog.Quant{{URL: "https://evil.example/y.gguf"}},
			},
			kind: modelsource.KindURL,
		},
		{
			name: "claims a known publisher, one file of several elsewhere",
			m: catalog.ModelSpec{
				ID: "hub:z", Source: catalog.Source{Registry: "huggingface", URL: pub},
				Quants: []catalog.Quant{{Files: []catalog.QuantFile{
					{URL: pub + "/resolve/main/a.gguf"}, {URL: "https://evil.example/b.gguf"},
				}}},
			},
			kind: modelsource.KindURL,
		},
		{
			name: "ollama registry",
			m: catalog.ModelSpec{
				ID: "ollama.com/library/x:latest", Source: catalog.Source{Registry: "ollama", URL: "https://registry.ollama.com/library/x"},
				Quants: []catalog.Quant{{URL: "https://registry.ollama.com/v2/library/x/blobs/sha256:00"}},
			},
			kind: modelsource.KindURL,
		},
		{
			name: "source url is an embedded catalog id",
			m: catalog.ModelSpec{
				ID: "sneaky", Source: catalog.Source{URL: embeddedLocalID(t)},
				Quants: []catalog.Quant{{URL: "https://evil.example/s.gguf"}},
			},
			kind: modelsource.KindFile,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := userEntrySource(c.m)
			if src.Kind != c.kind {
				t.Fatalf("kind = %v, want %v", src.Kind, c.kind)
			}
			if got := modelsource.Classify(src, known).Trust; got == sandbox.TrustTrusted {
				t.Errorf("a user entry was classified %v", got)
			}
		})
	}
}

func TestParseModelRefClassifiesAUserEntryByOrigin(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")
	if err := runModelAdd([]string{"--yes", src}, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := parseModelRef("local:tiny")
	if err != nil || got.Kind != modelsource.KindFile {
		t.Fatalf("parseModelRef = %+v, %v; want a file source", got, err)
	}
	cat, err := parseModelRef(embeddedLocalID(t))
	if err != nil || cat.Kind != modelsource.KindCatalog {
		t.Fatalf("an embedded id parsed as %+v, %v; want the catalog source", cat, err)
	}
}

func TestDamagedOverlayCostsNothingButItself(t *testing.T) {
	dir := overlayDir(t)
	if err := os.WriteFile(catalog.OverlayPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	cat, notes, err := loadCatalogNotes()
	if err != nil {
		t.Fatalf("a damaged overlay failed the load: %v", err)
	}
	if len(cat.Models) != len(base.Models) {
		t.Errorf("models = %d, want the %d embedded ones", len(cat.Models), len(base.Models))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], catalog.OverlayFile) {
		t.Errorf("notes = %q, want one naming %s", notes, catalog.OverlayFile)
	}
}

func TestRefusedOverlayEntryIsReportedNotLoaded(t *testing.T) {
	dir := overlayDir(t)
	id := embeddedLocalID(t)
	body, err := json.Marshal(catalog.Overlay{Models: []catalog.ModelSpec{{ID: id, Name: "impostor", Kind: catalog.KindLocal}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.OverlayPath(dir), body, 0o600); err != nil {
		t.Fatal(err)
	}
	cat, notes, err := loadCatalogNotes()
	if err != nil {
		t.Fatal(err)
	}
	m, _ := findModel(cat, id)
	if m.Name == "impostor" {
		t.Error("an overlay entry replaced an embedded one")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], id) {
		t.Errorf("notes = %q, want one naming %q", notes, id)
	}
}

func TestBlessSavesToTheOverlay(t *testing.T) {
	dir := overlayDir(t)
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "acme/tiny-1B", "cardData": map[string]any{"license": "mit"}},
		tree: []map[string]any{blessLFS("model.safetensors", 2048, strings.Repeat("11", 32))},
	})
	var out bytes.Buffer
	if err := modelBless(context.Background(), hub, dl, []string{"hf:acme/tiny-1B", "--id", "vllm:overlay-tiny"}, &out); err != nil {
		t.Fatalf("bless: %v\n%s", err, out.String())
	}
	m := userEntry(t, "vllm:overlay-tiny")
	if m.Trust != catalog.TrustLocal {
		t.Errorf("trust = %q; a blessed entry written by the user must be held local", m.Trust)
	}
	if strings.Contains(out.String(), "catalog/models.json") {
		t.Errorf("output still tells the user to edit models.json:\n%s", out.String())
	}
	if !strings.Contains(out.String(), catalog.OverlayPath(dir)) {
		t.Errorf("output does not say where the entry was saved:\n%s", out.String())
	}
}

func TestBlessRefusesAnEmbeddedID(t *testing.T) {
	dir := overlayDir(t)
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "acme/tiny-1B", "cardData": map[string]any{"license": "mit"}},
		tree: []map[string]any{blessLFS("model.safetensors", 2048, strings.Repeat("11", 32))},
	})
	id := embeddedLocalID(t)
	err := modelBless(context.Background(), hub, dl, []string{"hf:acme/tiny-1B", "--id", id}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("err = %v, want a refusal naming %q", err, id)
	}
	if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
		t.Error("a refused bless wrote the overlay")
	}
}

func TestBlessWithoutADataDirPrintsTheEntry(t *testing.T) {
	prev := catalogDataDir
	setCatalogDataDir("")
	t.Cleanup(func() { setCatalogDataDir(prev) })
	var out bytes.Buffer
	spec := catalog.ModelSpec{ID: "vllm:nowhere", Name: "nowhere"}
	if err := saveBlessedEntry("", spec, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"vllm:nowhere"`) {
		t.Errorf("output = %q, want the entry as JSON", out.String())
	}
}
