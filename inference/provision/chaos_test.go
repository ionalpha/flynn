package provision

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

// The archive-extraction security invariants (traversal refusal, decompression-bomb
// ceiling, safe-join containment) live with the generic extractor in the acquire package.
// This file keeps the install-level chaos invariants that are specific to the runtime
// provisioner: a failed fetch or a corrupt release leaves no usable build behind.
