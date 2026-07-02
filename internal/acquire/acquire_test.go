package acquire

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
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/fetch"
)

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

// serveArchive starts a TLS server returning body and a downloader whose client trusts it
// and may reach loopback, the same pattern the fetch tests use to exercise the hardened
// download path against a local server.
func serveArchive(t *testing.T, body []byte) (string, *fetch.Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, fetch.New(fetch.WithHTTPClient(srv.Client()))
}

func TestInstallToZip(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"flyctl.exe":    "#!binary",
		"wintun.dll":    "lib",
		"sub/notes.txt": "ignore",
	})
	url, dl := serveArchive(t, archive)
	rel := Release{URL: url, SHA256: sha256Hex(archive), SizeBytes: int64(len(archive)), Archive: ArchiveZip, BinName: "flyctl.exe"}
	target := filepath.Join(t.TempDir(), "flyctl", "v1")

	got, reused, err := InstallTo(context.Background(), dl, rel, target)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if reused {
		t.Fatal("first install should not be reused")
	}
	if filepath.Base(got) != "flyctl.exe" {
		t.Fatalf("binary path %q does not end in the binary name", got)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != "#!binary" {
		t.Fatalf("binary content wrong: %q err=%v", b, err)
	}
	// The sibling file must be extracted next to the binary so it runs in place.
	if _, err := os.Stat(filepath.Join(filepath.Dir(got), "wintun.dll")); err != nil {
		t.Fatalf("sibling file not extracted: %v", err)
	}

	// A second install reuses the directory with no download.
	again, reused, err := InstallTo(context.Background(), dl, rel, target)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !reused || again != got {
		t.Fatalf("second install should be reused at the same path, got reused=%v %q", reused, again)
	}
}

func TestInstallToTarGz(t *testing.T) {
	archive := buildTarGz(t, map[string]string{
		"bin/flyctl":  "elf",
		"bin/libx.so": "so",
	})
	url, dl := serveArchive(t, archive)
	rel := Release{URL: url, SHA256: sha256Hex(archive), SizeBytes: int64(len(archive)), Archive: ArchiveTarGz, BinName: "flyctl"}
	got, _, err := InstallTo(context.Background(), dl, rel, filepath.Join(t.TempDir(), "flyctl", "v1"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	// The binary is marked executable so it can be launched.
	if runtime.GOOS != "windows" && info.Mode()&0o100 == 0 {
		t.Fatalf("binary not executable: mode %v", info.Mode())
	}
}

func TestInstallToRefusesWrongDigest(t *testing.T) {
	archive := buildZip(t, map[string]string{"flyctl.exe": "real"})
	url, dl := serveArchive(t, archive)
	rel := Release{URL: url, SHA256: sha256Hex([]byte("a different archive")), SizeBytes: int64(len(archive)), Archive: ArchiveZip, BinName: "flyctl.exe"}
	target := filepath.Join(t.TempDir(), "flyctl", "v1")
	if _, _, err := InstallTo(context.Background(), dl, rel, target); err == nil {
		t.Fatal("expected a digest mismatch to fail the install")
	}
	if _, ok := FindBinary(target, "flyctl.exe"); ok {
		t.Fatal("a refused install must leave no binary on disk")
	}
}

// TestInstallToCorruptArchive covers bytes that verify against their digest but are not a
// valid archive: extraction fails and nothing is installed at the target.
func TestInstallToCorruptArchive(t *testing.T) {
	garbage := []byte("this is not a gzip stream at all, just bytes")
	url, dl := serveArchive(t, garbage)
	target := filepath.Join(t.TempDir(), "flyctl", "v1")
	rel := Release{URL: url, SHA256: sha256Hex(garbage), SizeBytes: int64(len(garbage)), Archive: ArchiveTarGz, BinName: "flyctl"}
	if _, _, err := InstallTo(context.Background(), dl, rel, target); err == nil {
		t.Fatal("a corrupt archive must error")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a failed install must leave no directory at the target (stat err=%v)", err)
	}
}

// TestExtractRefusesTraversalTar crafts a tar whose entry name escapes the install
// directory and asserts extraction refuses it and writes nothing outside the destination.
func TestExtractRefusesTraversalTar(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	payload := []byte("pwned")
	for _, name := range []string{"../escape.txt", "../../escape.txt", "/abs/escape.txt"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gw.Close()

	parent := t.TempDir()
	destDir := filepath.Join(parent, "install")
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(parent, "r.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := extract(ArchiveTarGz, tmp, destDir); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("traversal entry must be refused, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("a traversal entry escaped the install directory")
	}
}

// TestWriteFileEnforcesSizeCeiling covers the decompression-bomb guard at the unit level:
// a body larger than the remaining budget is refused rather than written.
func TestWriteFileEnforcesSizeCeiling(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := writeFile(dst, bytes.NewReader(bytes.Repeat([]byte{1}, 100)), 10); err == nil {
		t.Fatal("a body over the limit must be refused")
	}
	if _, err := writeFile(dst, bytes.NewReader([]byte("ok")), 0); err == nil {
		t.Fatal("a zero remaining budget must be refused")
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	base := filepath.Clean(filepath.Join(os.TempDir(), "install"))
	bad := []string{"../escape", "../../etc/passwd", "a/../../b", `..\windows`, "/abs/outside"}
	for _, name := range bad {
		if dst, ok := SafeJoin(base, name); ok {
			t.Fatalf("SafeJoin(%q) allowed escape to %q", name, dst)
		}
	}
	good := []string{"bin/flyctl", "a/b/c.txt"}
	for _, name := range good {
		if _, ok := SafeJoin(base, name); !ok {
			t.Fatalf("SafeJoin(%q) rejected a safe path", name)
		}
	}
}
