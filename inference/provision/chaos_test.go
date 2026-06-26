package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference"
)

// faultyBody yields data until failAfter bytes, then injects a read error: a download
// that dies mid-flight.
type faultyBody struct {
	data      []byte
	pos       int
	failAfter int
}

func (b *faultyBody) Read(p []byte) (int, error) {
	if b.pos >= b.failAfter {
		return 0, errors.New("injected mid-stream network failure")
	}
	end := b.pos + len(p)
	if end > b.failAfter {
		end = b.failAfter
	}
	if end > len(b.data) {
		end = len(b.data)
	}
	n := copy(p, b.data[b.pos:end])
	b.pos += n
	if n == 0 {
		return 0, errors.New("injected mid-stream network failure")
	}
	return n, nil
}

func (b *faultyBody) Close() error { return nil }

type faultyTransport struct {
	data      []byte
	failAfter int
}

func (ft faultyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    200,
		Body:          &faultyBody{data: ft.data, failAfter: ft.failAfter},
		ContentLength: int64(len(ft.data)),
		Header:        make(http.Header),
		Request:       r,
	}, nil
}

func buildVersion() inference.Version { return inference.Version{9813} }

// noBuildLeftBehind asserts the versioned build directory was never created, so a
// failed install can never be mistaken for a usable runtime.
func noBuildLeftBehind(t *testing.T, dest string) {
	t.Helper()
	buildDir := filepath.Join(dest, "llama.cpp", buildVersion().String())
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Fatalf("a failed install must leave no build at %s (stat err=%v)", buildDir, err)
	}
}

// TestInstallMidStreamFailureLeavesNothing is the chaos invariant for a download that
// dies partway through: Install fails and no build is left behind.
func TestInstallMidStreamFailureLeavesNothing(t *testing.T) {
	full := bytes.Repeat([]byte{0xab}, 8192)
	dl := fetch.New(fetch.WithHTTPClient(&http.Client{Transport: faultyTransport{data: full, failAfter: 1000}}))
	dest := t.TempDir()
	rel := Release{
		Runtime: "llama.cpp", Version: buildVersion(), GOOS: "linux", GOARCH: "amd64",
		URL: "https://example.com/r.tar.gz", SHA256: sha256Hex(full), SizeBytes: int64(len(full)),
		Archive: ArchiveTarGz, BinName: "llama-server",
	}
	if _, err := Install(context.Background(), dl, rel, dest); err == nil {
		t.Fatal("a mid-stream failure must error")
	}
	noBuildLeftBehind(t, dest)
}

// TestInstallCorruptArchiveLeavesNothing covers bytes that verify against their digest
// but are not a valid archive (a corrupt or wrong-format release): extraction fails and
// nothing is installed.
func TestInstallCorruptArchiveLeavesNothing(t *testing.T) {
	garbage := []byte("this is not a gzip stream at all, just bytes")
	url, dl := serveArchive(t, garbage)
	dest := t.TempDir()
	rel := Release{
		Runtime: "llama.cpp", Version: buildVersion(), GOOS: "linux", GOARCH: "amd64",
		URL: url, SHA256: sha256Hex(garbage), SizeBytes: int64(len(garbage)),
		Archive: ArchiveTarGz, BinName: "llama-server",
	}
	if _, err := Install(context.Background(), dl, rel, dest); err == nil {
		t.Fatal("a corrupt archive must error")
	}
	noBuildLeftBehind(t, dest)
}

// TestExtractRefusesTraversalTar crafts a tar whose entry name escapes the install
// directory (the tar writer, unlike the zip writer, allows arbitrary names), and
// asserts extraction refuses it and writes nothing outside the destination.
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
	// Nothing may have been written above the install directory.
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("a traversal entry escaped the install directory")
	}
}

// TestWriteFileEnforcesSizeCeiling covers the decompression-bomb guard at the unit
// level: a body larger than the remaining budget is refused rather than written.
func TestWriteFileEnforcesSizeCeiling(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := writeFile(dst, bytes.NewReader(bytes.Repeat([]byte{1}, 100)), 10); err == nil {
		t.Fatal("a body over the limit must be refused")
	}
	if _, err := writeFile(dst, bytes.NewReader([]byte("ok")), 0); err == nil {
		t.Fatal("a zero remaining budget must be refused")
	}
}
