package acquire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/internal/fetch"
)

// serveByPath serves each body at its path and returns the base URL with a downloader that
// trusts the server.
func serveByPath(t *testing.T, bodies map[string][]byte) (string, *fetch.Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, fetch.New(fetch.WithHTTPClient(srv.Client()))
}

func TestInstallToUnpacksCompanionsBesideTheBinary(t *testing.T) {
	main := buildZip(t, map[string]string{"server.exe": "bin"})
	libs := buildZip(t, map[string]string{"cudart.dll": "lib"})
	base, dl := serveByPath(t, map[string][]byte{"/main.zip": main, "/libs.zip": libs})
	rel := Release{
		URL: base + "/main.zip", SHA256: sha256Hex(main), SizeBytes: int64(len(main)), Archive: ArchiveZip, BinName: "server.exe",
		Companions: []Companion{{URL: base + "/libs.zip", SHA256: sha256Hex(libs), SizeBytes: int64(len(libs)), Archive: ArchiveZip}},
	}
	target := filepath.Join(t.TempDir(), "rt", "v1")

	bin, reused, err := InstallTo(context.Background(), dl, rel, target)
	if err != nil || reused {
		t.Fatalf("install: reused=%v err=%v", reused, err)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "cudart.dll")); err != nil || string(b) != "lib" {
		t.Fatalf("companion not beside the binary: %q %v", b, err)
	}
}

func TestInstallToRefusesATamperedCompanionAndLeavesNothing(t *testing.T) {
	main := buildZip(t, map[string]string{"server.exe": "bin"})
	libs := buildZip(t, map[string]string{"cudart.dll": "lib"})
	evil := buildZip(t, map[string]string{"cudart.dll": "evil"})
	base, dl := serveByPath(t, map[string][]byte{"/main.zip": main, "/libs.zip": evil})
	rel := Release{
		URL: base + "/main.zip", SHA256: sha256Hex(main), SizeBytes: int64(len(main)), Archive: ArchiveZip, BinName: "server.exe",
		Companions: []Companion{{URL: base + "/libs.zip", SHA256: sha256Hex(libs), SizeBytes: int64(len(evil)), Archive: ArchiveZip}},
	}
	target := filepath.Join(t.TempDir(), "rt", "v1")

	if _, _, err := InstallTo(context.Background(), dl, rel, target); err == nil {
		t.Fatal("a companion that does not match its digest was installed")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a refused companion must leave no directory at the target (stat err=%v)", err)
	}
}

// An archive that cannot be staged is reported before anything is requested: the download
// goes through a file beside the install directory, and a directory that is gone has none.
func TestFetchAndExtractFailsWhenTheDownloadCannotBeStaged(t *testing.T) {
	_, dl := serveByPath(t, nil)
	gone := filepath.Join(t.TempDir(), "missing")
	err := fetchAndExtract(t.Context(), dl, gone, t.TempDir(), "https://127.0.0.1:1/x", "00", 1, ArchiveZip)
	if err == nil {
		t.Fatal("an archive was fetched into a directory that does not exist")
	}
}
