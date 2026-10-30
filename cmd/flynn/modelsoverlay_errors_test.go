package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
)

// plainEmbeddedID is the id of an embedded local model with no colon in it. `--id` refuses a
// colon, so only such an id can collide with an embedded entry through the add flags.
func plainEmbeddedID(t *testing.T) string {
	t.Helper()
	base, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range base.Models {
		if m.Local() && !strings.Contains(m.ID, ":") {
			return m.ID
		}
	}
	t.Fatal("the embedded catalog has no local model whose id lacks a colon")
	return ""
}

func TestAddRefusesAnEmbeddedIDBeforeCopyingOrAsking(t *testing.T) {
	id := plainEmbeddedID(t)
	good := blessRepo{
		info: map[string]any{"id": "acme/m"},
		tree: []map[string]any{blessLFS("m-Q4_K_M.gguf", 10, strings.Repeat("ab", 32))},
	}
	for name, add := range map[string]func(dir string) error{
		"local file": func(dir string) error {
			src, _ := writeGGUF(t, "tiny.gguf")
			return runModelAdd([]string{"--yes", "--id", id, src}, dir, &bytes.Buffer{})
		},
		"hub": func(dir string) error {
			hub, _ := newBlessHub(t, good)
			return addHub(context.Background(), hub, dir, []string{"hf:acme/m", "--id", id}, true, &bytes.Buffer{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := overlayDir(t)
			err := add(dir)
			var refusal catalog.RefusalError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), id) {
				t.Fatalf("err = %v, want a refusal naming %q", err, id)
			}
			if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
				t.Error("a refused add wrote the overlay")
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "models")); len(entries) != 0 {
				t.Error("a refused add copied weights")
			}
		})
	}
}

func TestAddHubStopsOnEachFailure(t *testing.T) {
	good := []map[string]any{blessLFS("m-Q4_K_M.gguf", 10, strings.Repeat("ab", 32))}
	cases := map[string]struct {
		repo blessRepo
		args []string
		yes  bool
	}{
		"two sources":           {blessRepo{tree: good}, []string{"hf:acme/m", "hf:acme/n"}, true},
		"model card unreadable": {blessRepo{infoErr: http.StatusInternalServerError, tree: good}, []string{"hf:acme/m"}, true},
		"file list unreadable":  {blessRepo{info: map[string]any{}, treeErr: http.StatusInternalServerError}, []string{"hf:acme/m"}, true},
		"consent declined":      {blessRepo{info: map[string]any{}, tree: good}, []string{"hf:acme/m"}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := overlayDir(t)
			hub, _ := newBlessHub(t, c.repo)
			if err := addHub(context.Background(), hub, dir, c.args, c.yes, &bytes.Buffer{}); err == nil {
				t.Fatal("expected an error")
			}
			if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
				t.Error("a failed add wrote the overlay")
			}
		})
	}
}

func TestAddingTheSameModelAgainUpdatesIt(t *testing.T) {
	dir := overlayDir(t)
	src, _ := writeGGUF(t, "tiny.gguf")
	for i, want := range []string{"added local:tiny", "updated local:tiny"} {
		var out bytes.Buffer
		if err := runModelAdd([]string{"--yes", src}, dir, &out); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("add %d output = %q, want %q", i, out.String(), want)
		}
	}
}

func TestAddLocalFileRefusals(t *testing.T) {
	t.Run("safetensors file", func(t *testing.T) {
		dir := overlayDir(t)
		weights := filepath.Join(t.TempDir(), "w.safetensors")
		if err := os.WriteFile(weights, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runModelAdd([]string{"--yes", weights}, dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "only GGUF files") {
			t.Fatalf("err = %v, want a refusal of a file that is not GGUF", err)
		}
	})

	t.Run("weights cannot be copied", func(t *testing.T) {
		dir := overlayDir(t)
		// A file where the models directory belongs.
		if err := os.WriteFile(filepath.Join(dir, "models"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		src, _ := writeGGUF(t, "tiny.gguf")
		if err := runModelAdd([]string{"--yes", src}, dir, &bytes.Buffer{}); err == nil {
			t.Fatal("expected the copy to fail")
		}
		if _, statErr := os.Stat(catalog.OverlayPath(dir)); statErr == nil {
			t.Error("a failed copy wrote the overlay")
		}
	})
}

func TestCopyIntoModelsFailures(t *testing.T) {
	src, sum := writeGGUF(t, "tiny.gguf")

	t.Run("source is missing", func(t *testing.T) {
		if _, err := copyIntoModels(t.TempDir(), filepath.Join(t.TempDir(), "absent.gguf"), "w.gguf"); err == nil {
			t.Error("copied a file that does not exist")
		}
	})

	t.Run("destination is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "models", "w.gguf"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := copyIntoModels(dir, src, "w.gguf"); err == nil {
			t.Error("replaced a directory with weights")
		}
	})

	t.Run("copy returns the digest of the bytes written", func(t *testing.T) {
		got, err := copyIntoModels(t.TempDir(), src, "w.gguf")
		if err != nil || got != sum {
			t.Errorf("digest = %q, %v; want %q", got, err, sum)
		}
	})
}

func TestAddOllamaStopsOnEachFailure(t *testing.T) {
	t.Run("two sources", func(t *testing.T) {
		dir := overlayDir(t)
		if err := runModelAdd([]string{"--yes", "ollama:tiny", "ollama:other"}, dir, &bytes.Buffer{}); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("registry address is not a URL", func(t *testing.T) {
		dir := overlayDir(t)
		prev := ollamaRegistry
		ollamaRegistry = "http://[::1"
		t.Cleanup(func() { ollamaRegistry = prev })
		if err := runModelAdd([]string{"--yes", "ollama:tiny"}, dir, &bytes.Buffer{}); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("registry is unreachable", func(t *testing.T) {
		dir := overlayDir(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		prevReg, prevClient := ollamaRegistry, ollamaHTTPClient
		ollamaRegistry = url
		ollamaHTTPClient = func() *http.Client { return http.DefaultClient }
		t.Cleanup(func() { ollamaRegistry, ollamaHTTPClient = prevReg, prevClient })
		err := runModelAdd([]string{"--yes", "ollama:tiny"}, dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "read the ollama manifest") {
			t.Errorf("err = %v, want the manifest read to fail", err)
		}
	})

	t.Run("manifest is not a manifest", func(t *testing.T) {
		dir := overlayDir(t)
		ollamaStub(t, "this is not a manifest", http.StatusOK)
		err := runModelAdd([]string{"--yes", "ollama:tiny"}, dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "decode the ollama manifest") {
			t.Errorf("err = %v, want the decode to fail", err)
		}
	})
}

func TestModelsCommandDispatchesAddAndRemove(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeGGUF(t, "tiny.gguf")
	if err := dispatchModels([]string{"add", "--yes", src}, dir); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := dispatchModels([]string{"remove", "local:tiny"}, dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := dispatchModels([]string{"remove", "local:tiny"}, dir); err == nil {
		t.Error("removing an entry twice succeeded")
	}
}

func TestRunModelsPrintsWhatTheOverlayLost(t *testing.T) {
	dir := overlayDir(t)
	if err := os.WriteFile(catalog.OverlayPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runModels(nil, dir, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "note: ignored "+catalog.OverlayFile) {
		t.Errorf("output does not say the overlay was ignored:\n%s", out.String())
	}
}

func TestLoadCatalogNotesFailsWhenTheEmbeddedListDoes(t *testing.T) {
	prev := loadWithOverlay
	loadWithOverlay = func(string) (catalog.Catalog, []catalog.RefusalError, error) {
		return catalog.Catalog{}, nil, errors.New("embedded catalog is broken")
	}
	t.Cleanup(func() { loadWithOverlay = prev })
	if _, _, err := loadCatalogNotes(); err == nil {
		t.Error("an unloadable embedded catalog was reported as a note")
	}
	if _, ok := userModelEntry("local:tiny"); ok {
		t.Error("an entry was found in a catalog that failed to load")
	}
}

func TestUserEntrySourceFallsBackForAnUnreadableOrigin(t *testing.T) {
	m := catalog.ModelSpec{ID: "hub:odd", Source: catalog.Source{Registry: "huggingface", URL: ""}}
	if got := userEntrySource(m); got.Kind != modelsource.KindFile {
		t.Errorf("kind = %v, want a file source for an entry with no origin", got.Kind)
	}
	noURLs := catalog.ModelSpec{
		ID:     "hub:nourl",
		Source: catalog.Source{Registry: "huggingface", URL: "https://huggingface.co/rando/x"},
		Quants: []catalog.Quant{{Name: "Q4_K_M"}},
	}
	if got := userEntrySource(noURLs); got.Kind != modelsource.KindURL || got.URL != "" {
		t.Errorf("source = %+v, want a URL source with no URL for an entry that names no download", got)
	}
}

func TestConfirmAddWeighsTheHubPublisher(t *testing.T) {
	m := catalog.ModelSpec{
		ID:     "hub:x",
		Source: catalog.Source{Registry: "huggingface", URL: "https://huggingface.co/rando/x"},
		Quants: []catalog.Quant{{URL: "https://huggingface.co/rando/x/resolve/main/x.gguf"}},
	}
	var out bytes.Buffer
	if err := confirmAdd(&out, userEntrySource(m), true); err != nil {
		t.Fatalf("confirmAdd: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "rando") {
		t.Errorf("output does not name the publisher:\n%s", out.String())
	}
}

func TestRemoveSkipsMultiFileQuantWeights(t *testing.T) {
	dir := overlayDir(t)
	entry := catalog.ModelSpec{
		ID: "hub:sharded", Name: "sharded", Kind: catalog.KindLocal, Trust: catalog.TrustLocal,
		Quants: []catalog.Quant{{Name: "bf16", Format: catalog.FormatSafetensors, Files: []catalog.QuantFile{{Name: "a.safetensors", URL: "https://huggingface.co/acme/s/resolve/main/a.safetensors"}}}},
	}
	body, err := json.Marshal(catalog.Overlay{Models: []catalog.ModelSpec{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.OverlayPath(dir), body, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runModelRemove([]string{entry.ID}, dir, &out); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out.String(), "0 downloaded file") {
		t.Errorf("output = %q, want no file deleted for a multi-file quant", out.String())
	}
}

func TestSaveBlessedEntryReportsEachFailure(t *testing.T) {
	spec := userSpec("vllm:saved", "saved", 1,
		catalog.Source{Publisher: "acme", URL: "https://huggingface.co/acme/saved", Registry: "huggingface"},
		"mit", "chatml",
		catalog.Quant{Name: "Q4_K_M", Format: catalog.FormatGGUF, SizeBytes: 10, URL: "https://huggingface.co/acme/saved/resolve/main/s.gguf", Digest: "sha256:" + strings.Repeat("ab", 32)})

	t.Run("printing fails", func(t *testing.T) {
		if err := saveBlessedEntry("", spec, failingWriter{}); err == nil {
			t.Error("a failed print was reported as saved")
		}
	})

	t.Run("overlay cannot be read", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(catalog.OverlayPath(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := saveBlessedEntry(dir, spec, &bytes.Buffer{}); err == nil {
			t.Error("saved over an unreadable overlay")
		}
	})

	t.Run("saving twice says updated", func(t *testing.T) {
		dir := t.TempDir()
		for i, want := range []string{"saved vllm:saved", "updated vllm:saved"} {
			var out bytes.Buffer
			if err := saveBlessedEntry(dir, spec, &out); err != nil {
				t.Fatalf("save %d: %v", i, err)
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("save %d output = %q, want %q", i, out.String(), want)
			}
		}
	})
}
