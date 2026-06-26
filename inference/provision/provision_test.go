package provision

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference"
)

// buildZip returns a zip archive whose entries are the given name->content map, plus
// the optional extra raw entries (used to inject a traversal path the writer would
// otherwise clean).
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serveArchive starts a TLS server returning body and a downloader whose client trusts
// it and may reach loopback, the same pattern the fetch tests use to exercise the
// hardened download path against a local server.
func serveArchive(t *testing.T, body []byte) (string, *fetch.Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, fetch.New(fetch.WithHTTPClient(srv.Client()))
}

func TestInstallZip(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"llama-server.exe": "#!binary",
		"ggml.dll":         "lib",
		"sub/notes.txt":    "ignore",
	})
	url, dl := serveArchive(t, archive)
	rel := Release{
		Runtime: "llama.cpp", Version: inference.Version{9813}, GOOS: "windows", GOARCH: "amd64",
		URL: url, SHA256: sha256Hex(archive), SizeBytes: int64(len(archive)),
		Archive: ArchiveZip, BinName: "llama-server.exe",
	}
	dest := t.TempDir()

	got, err := Install(context.Background(), dl, rel, dest)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got.FromCache {
		t.Fatal("first install should not be from cache")
	}
	if filepath.Base(got.BinPath) != "llama-server.exe" {
		t.Fatalf("binary path %q does not end in the binary name", got.BinPath)
	}
	if b, err := os.ReadFile(got.BinPath); err != nil || string(b) != "#!binary" {
		t.Fatalf("binary content wrong: %q err=%v", b, err)
	}
	// The sibling library must be extracted next to the binary so it runs in place.
	if _, err := os.Stat(filepath.Join(filepath.Dir(got.BinPath), "ggml.dll")); err != nil {
		t.Fatalf("sibling library not extracted: %v", err)
	}

	// A second install reuses the build with no download (the server is closed by
	// cleanup only at test end, but a cache hit must not touch it at all).
	again, err := Install(context.Background(), dl, rel, dest)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !again.FromCache {
		t.Fatal("second install should be served from cache")
	}
}

func TestInstallTarGz(t *testing.T) {
	archive := buildTarGz(t, map[string]string{
		"build/bin/llama-server": "elf",
		"build/bin/libggml.so":   "so",
	})
	url, dl := serveArchive(t, archive)
	rel := Release{
		Runtime: "llama.cpp", Version: inference.Version{9813}, GOOS: "linux", GOARCH: "amd64",
		URL: url, SHA256: sha256Hex(archive), SizeBytes: int64(len(archive)),
		Archive: ArchiveTarGz, BinName: "llama-server",
	}
	got, err := Install(context.Background(), dl, rel, t.TempDir())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if filepath.Base(got.BinPath) != "llama-server" {
		t.Fatalf("binary path %q wrong", got.BinPath)
	}
	info, err := os.Stat(got.BinPath)
	if err != nil {
		t.Fatal(err)
	}
	// The binary is marked executable so it can be launched.
	if runtime.GOOS != "windows" && info.Mode()&0o100 == 0 {
		t.Fatalf("binary not executable: mode %v", info.Mode())
	}
}

func TestInstallRefusesWrongDigest(t *testing.T) {
	archive := buildZip(t, map[string]string{"llama-server.exe": "real"})
	url, dl := serveArchive(t, archive)
	rel := Release{
		Runtime: "llama.cpp", Version: inference.Version{9813}, GOOS: "windows", GOARCH: "amd64",
		URL: url, SHA256: sha256Hex([]byte("a different archive")), SizeBytes: int64(len(archive)),
		Archive: ArchiveZip, BinName: "llama-server.exe",
	}
	dest := t.TempDir()
	if _, err := Install(context.Background(), dl, rel, dest); err == nil {
		t.Fatal("expected a digest mismatch to fail the install")
	}
	// Nothing must be left at the build location after a refused download.
	if _, err := findBinaryInDest(dest, "llama-server.exe"); err == nil {
		t.Fatal("a refused install must leave no binary on disk")
	}
}

func TestInstallRefusesVulnerableVersion(t *testing.T) {
	// A build below the llama.cpp floor must be refused before any download, so the
	// URL is deliberately unreachable: reaching it would be the bug.
	rel := Release{
		Runtime: "llama.cpp", Version: inference.Version{1}, GOOS: "linux", GOARCH: "amd64",
		URL: "https://127.0.0.1:1/never", SHA256: "00", SizeBytes: 1,
		Archive: ArchiveTarGz, BinName: "llama-server",
	}
	if err := rel.Gate(); err == nil {
		t.Fatal("a sub-floor version must not pass the gate")
	}
	if _, err := Install(context.Background(), fetch.New(), rel, t.TempDir()); err == nil {
		t.Fatal("Install must refuse a sub-floor version without fetching")
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	base := filepath.Clean("/tmp/install")
	bad := []string{"../escape", "../../etc/passwd", "a/../../b", `..\windows`, "/abs/outside"}
	for _, name := range bad {
		if dst, ok := safeJoin(base, name); ok {
			t.Fatalf("safeJoin(%q) allowed escape to %q", name, dst)
		}
	}
	good := map[string]string{"bin/llama-server": "bin", "a/b/c.txt": "c.txt"}
	for name := range good {
		if _, ok := safeJoin(base, name); !ok {
			t.Fatalf("safeJoin(%q) rejected a safe path", name)
		}
	}
}

func TestRegistryReleasesPassGate(t *testing.T) {
	if len(Releases()) == 0 {
		t.Fatal("expected pinned releases")
	}
	for _, r := range Releases() {
		if err := r.Gate(); err != nil {
			t.Fatalf("pinned release %s/%s %s fails the gate: %v", r.GOOS, r.GOARCH, r.Version, err)
		}
		if r.SHA256 == "" || r.URL == "" || r.BinName == "" {
			t.Fatalf("pinned release %s/%s is missing a field", r.GOOS, r.GOARCH)
		}
	}
	// The current platform must have a build to install, so provisioning is never a
	// dead end where Flynn runs.
	if _, ok := ReleaseFor("llama.cpp", runtime.GOOS, runtime.GOARCH); !ok {
		t.Fatalf("no pinned llama.cpp build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// findBinaryInDest reports an error when the named binary is absent anywhere under
// dest, for asserting that a refused install left nothing behind.
func findBinaryInDest(dest, name string) (string, error) {
	if p, ok := findBinary(dest, name); ok {
		return p, nil
	}
	return "", os.ErrNotExist
}
