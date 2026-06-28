package dependency

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/resource"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return NewStore(resource.NewMemory(reg))
}

// fakeProber scripts a version string (or an error) for any probe, standing in for running
// a real program through the sandbox.
type fakeProber struct {
	out string
	err error
}

func (p fakeProber) Probe(context.Context, string, []string) (string, error) { return p.out, p.err }

// absent is a prober that reports every program as not runnable.
var absent = fakeProber{err: errors.New("not found")}

// serveTarGz builds a tar.gz holding a single executable and serves it over TLS, returning
// the url, the downloader that trusts the server, and the archive's digest and size.
func serveTarGz(t *testing.T, binName, content string) (url string, dl *fetch.Downloader, sha string, size int64) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: binName, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()
	body := buf.Bytes()
	sum := sha256.Sum256(body)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, fetch.New(fetch.WithHTTPClient(srv.Client())), hex.EncodeToString(sum[:]), int64(len(body))
}

func linuxSpec(t *testing.T, url, sha string, size int64) Spec {
	t.Helper()
	return Spec{
		Binaries:     []string{"flyctl", "fly"},
		VersionArgs:  []string{"version"},
		VersionRegex: `v?(\d+\.\d+\.\d+)`,
		MinVersion:   "0.3.0",
		Pin:          "0.4.61",
		Releases: []Release{
			{GOOS: "linux", GOARCH: "amd64", URL: url, SHA256: sha, SizeBytes: size, Archive: "tar.gz", BinName: "flyctl"},
		},
	}
}

// TestResolveUsesSystemAboveFloor proves a present binary that meets the floor is used as-is,
// with no download (the release URL is unreachable, so reaching it would be the bug).
func TestResolveUsesSystemAboveFloor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://127.0.0.1:1/never", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(),
		WithProber(fakeProber{out: "flyctl v0.4.61 linux/amd64"}),
		WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceSystem || got.Path != "flyctl" || got.Version != "0.4.61" {
		t.Fatalf("expected the system binary, got %+v", got)
	}
}

// TestResolveSkipsSystemBelowFloor proves a present binary below the floor is not used; the
// pinned build is provisioned instead.
func TestResolveSkipsSystemBelowFloor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	url, dl, sha, size := serveTarGz(t, "flyctl", "#!flyctl")
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, url, sha, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, dl, t.TempDir(),
		WithProber(fakeProber{out: "flyctl v0.1.0 linux/amd64"}), // below the 0.3.0 floor
		WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceProvisioned || got.Version != "0.4.61" {
		t.Fatalf("expected a provisioned build, got %+v", got)
	}
}

// TestResolveProvisionsWhenAbsent proves a missing program is fetched, verified, and installed.
func TestResolveProvisionsWhenAbsent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	url, dl, sha, size := serveTarGz(t, "flyctl", "#!flyctl")
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, url, sha, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, dl, t.TempDir(), WithProber(absent), WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceProvisioned {
		t.Fatalf("expected provisioned, got %+v", got)
	}
	if b, err := os.ReadFile(got.Path); err != nil || string(b) != "#!flyctl" {
		t.Fatalf("installed binary content wrong: %q err=%v", b, err)
	}
}

// TestResolveNoBuildForPlatform proves an absent program with no shipped build for the host
// fails with a clear, actionable error rather than silently.
func TestResolveNoBuildForPlatform(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://x/y", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent), WithPlatform("plan9", "mips"))
	if _, err := m.Resolve(ctx, "flyctl"); err == nil {
		t.Fatal("expected a missing-build error for an unsupported platform")
	}
}

// TestResolvePinBelowFloorRefused proves a spec whose pinned version is below its own floor
// is refused rather than installing a build the spec itself says is too old.
func TestResolvePinBelowFloorRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	spec := linuxSpec(t, "https://x/y", "00", 1)
	spec.Pin = "0.2.0" // below the 0.3.0 floor
	if _, err := s.Put(ctx, "flyctl", spec); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent), WithPlatform("linux", "amd64"))
	if _, err := m.Resolve(ctx, "flyctl"); err == nil {
		t.Fatal("expected a pin-below-floor refusal")
	}
}

// TestCheckReportsState proves Check observes presence and floor compliance without
// provisioning.
func TestCheckReportsState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://x/y", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Present and above floor.
	m := NewManager(s, fetch.New(), t.TempDir(),
		WithProber(fakeProber{out: "flyctl v0.4.61 linux/amd64"}), WithPlatform("linux", "amd64"))
	rep, err := m.Check(ctx, "flyctl")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rep.Present || !rep.MeetsFloor || !rep.CanProvision {
		t.Fatalf("expected present+meets-floor+can-provision, got %+v", rep)
	}

	// Present but below floor.
	m2 := NewManager(s, fetch.New(), t.TempDir(),
		WithProber(fakeProber{out: "flyctl v0.1.0 linux/amd64"}), WithPlatform("linux", "amd64"))
	rep2, _ := m2.Check(ctx, "flyctl")
	if !rep2.Present || rep2.MeetsFloor {
		t.Fatalf("expected present-but-below-floor, got %+v", rep2)
	}

	// Absent.
	m3 := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent), WithPlatform("linux", "amd64"))
	rep3, _ := m3.Check(ctx, "flyctl")
	if rep3.Present {
		t.Fatalf("expected absent, got %+v", rep3)
	}
}

// TestProbeErrorSkipsBinary proves a binary whose probe fails is treated as not usable, not
// trusted blindly: the pinned build is provisioned instead.
func TestProbeErrorSkipsBinary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	url, dl, sha, size := serveTarGz(t, "flyctl", "#!flyctl")
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, url, sha, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, dl, t.TempDir(),
		WithProber(fakeProber{err: errors.New("exec format error")}),
		WithPlatform("linux", "amd64"))
	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceProvisioned {
		t.Fatalf("an unprobeable system binary must be skipped, got %+v", got)
	}
}
