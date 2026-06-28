package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fetch"
)

// serveFiles starts a TLS server serving a set of named files and a downloader that trusts
// it, the multi-file counterpart to serveArchive.
func serveFiles(t *testing.T, files map[string]string) (string, *fetch.Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, fetch.New(fetch.WithHTTPClient(srv.Client()))
}

func manifestFor(base string, files map[string]string) []ModelFile {
	var out []ModelFile
	for name, body := range files {
		out = append(out, ModelFile{
			Name: name, URL: base + "/" + name,
			SHA256: sha256Hex([]byte(body)), SizeBytes: int64(len(body)),
		})
	}
	return out
}

func TestFetchModelDir(t *testing.T) {
	files := map[string]string{
		"config.json":       `{"model_type":"qwen2"}`,
		"model.safetensors": "fake-weight-bytes",
		"tokenizer.json":    "{}",
	}
	base, dl := serveFiles(t, files)
	dir := filepath.Join(t.TempDir(), "model")

	got, err := FetchModelDir(context.Background(), dl, manifestFor(base, files), dir)
	if err != nil {
		t.Fatalf("FetchModelDir: %v", err)
	}
	if got != dir {
		t.Fatalf("returned dir %q, want %q", got, dir)
	}
	for name, body := range files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != body {
			t.Fatalf("file %s content %q err=%v", name, b, err)
		}
	}
	if !ModelDirPresent(manifestFor(base, files), dir) {
		t.Fatal("a fully fetched model should report present")
	}
	// A second fetch reuses present files and does not need the server (closed by cleanup at
	// test end, but a re-fetch must not depend on it).
	if _, err := FetchModelDir(context.Background(), dl, manifestFor(base, files), dir); err != nil {
		t.Fatalf("second fetch should be a no-op: %v", err)
	}
}

func TestFetchModelDirRefusesTraversal(t *testing.T) {
	base, dl := serveFiles(t, map[string]string{"x": "y"})
	bad := []ModelFile{{Name: "../escape.bin", URL: base + "/x", SHA256: sha256Hex([]byte("y"))}}
	if _, err := FetchModelDir(context.Background(), dl, bad, filepath.Join(t.TempDir(), "model")); err == nil {
		t.Fatal("a file name that escapes the model directory must be refused")
	}
}

func TestFetchModelDirRefusesBadDigest(t *testing.T) {
	files := map[string]string{"model.safetensors": "real-bytes"}
	base, dl := serveFiles(t, files)
	bad := []ModelFile{{Name: "model.safetensors", URL: base + "/model.safetensors", SHA256: sha256Hex([]byte("different"))}}
	dir := filepath.Join(t.TempDir(), "model")
	if _, err := FetchModelDir(context.Background(), dl, bad, dir); err == nil {
		t.Fatal("a digest mismatch must fail the fetch")
	}
}

func TestFetchModelDirEmptyManifest(t *testing.T) {
	if _, err := FetchModelDir(context.Background(), fetch.New(), nil, t.TempDir()); err == nil {
		t.Fatal("an empty manifest must be refused")
	}
}

func TestModelDirPresentPartial(t *testing.T) {
	files := map[string]string{"a": "1", "b": "2"}
	base, dl := serveFiles(t, files)
	dir := filepath.Join(t.TempDir(), "model")
	// Fetch only one file by hand, so the directory is partial.
	partial := []ModelFile{{Name: "a", URL: base + "/a", SHA256: sha256Hex([]byte("1")), SizeBytes: 1}}
	if _, err := FetchModelDir(context.Background(), dl, partial, dir); err != nil {
		t.Fatal(err)
	}
	if ModelDirPresent(manifestFor(base, files), dir) {
		t.Fatal("a directory missing a file must not report present")
	}
}
