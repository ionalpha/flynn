package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/release"
)

// goldenTag is the release whose real, published Sigstore bundle this package is
// tested against. The bundle is the one GitHub actually served.
const goldenTag = "v0.1.3-rc.1"

// buildFakeFlynn compiles a stand-in binary that reports the version it was built
// with, exactly as flynn does. The install path is tested by installing a real
// executable and running it, because every interesting failure in a self-update lives
// in the part that touches the disk and then executes what it wrote.
func buildFakeFlynn(t *testing.T, version string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("compiling the stand-in binary is too slow for -short")
	}

	src := t.TempDir()
	main := `package main

import "fmt"

var version = "dev"

func main() { fmt.Println(version) }
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fakeflynn\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(src, binaryFor(runtime.GOOS))
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the stand-in binary: %v\n%s", err, combined)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func binaryFor(goos string) string {
	if goos == "windows" {
		return "flynn.exe"
	}
	return "flynn"
}

// packArchive builds a release archive in the shape goreleaser produces: the binary,
// plus the incidental files that ride along with it.
func packArchive(t *testing.T, goos string, binary []byte) []byte {
	t.Helper()
	files := []struct {
		name string
		body []byte
		mode int64
	}{
		{"LICENSE", []byte("Apache-2.0"), 0o644},
		{binaryFor(goos), binary, 0o755},
		{"README.md", []byte("# flynn"), 0o644},
	}

	var buf bytes.Buffer
	if goos == "windows" {
		zw := zip.NewWriter(&buf)
		for _, f := range files {
			w, err := zw.Create(f.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(f.body); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// origin is a stand-in for GitHub: it serves a release listing and release assets.
// Tests reach into it to serve the wrong bytes, so the checks that are supposed to
// catch that get a chance to.
type origin struct {
	t       *testing.T
	listing string
	assets  map[string][]byte // path under the download base
	server  *httptest.Server
}

func newOrigin(t *testing.T) *origin {
	o := &origin{t: t, assets: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(o.listing))
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := o.assets[strings.TrimPrefix(r.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	o.server = httptest.NewTLSServer(mux)
	t.Cleanup(o.server.Close)
	return o
}

func (o *origin) listRelease(tags ...string) {
	var entries []string
	for _, tag := range tags {
		entries = append(entries, fmt.Sprintf(
			`{"tag_name":%q,"draft":false,"prerelease":%v,"published_at":"2026-07-08T06:32:17Z"}`,
			tag, strings.Contains(tag, "-")))
	}
	o.listing = "[" + strings.Join(entries, ",") + "]"
}

// updater wires an Updater to this fake origin, with the running binary standing in a
// temporary directory so the install path can be exercised for real.
func (o *origin) updater(t *testing.T, currentVersion string, exe string) *Updater {
	t.Helper()
	// The fake origin speaks TLS with its own certificate, and the Downloader refuses to
	// reach a private address. Both are correct in production and both have to be stood
	// down for a test that talks to a loopback server; the checks they perform have
	// their own tests, and the digest pinning under test here does not depend on either.
	client := o.server.Client()
	client.Timeout = 30 * time.Second

	return New(t.TempDir(),
		func(u *Updater) {
			u.http = client
			u.fetcher = fetch.New(fetch.WithHTTPClient(client))
			u.listingURL = o.server.URL + "/releases"
			u.downloadBase = o.server.URL + "/download/"
			u.version = currentVersion
			u.exe = func() (string, error) { return exe, nil }
			u.clock = clock.NewManual(time.Unix(1783492338, 0).UTC())
		})
}

// installedFlynn puts a stand-in flynn at a temporary path, as if it had been
// installed there, and returns its path.
func installedFlynn(t *testing.T, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, binaryFor(runtime.GOOS))
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func goldenBundle(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "v0.1.3-rc.1.intoto.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestUpgradeInstallsAVerifiedRelease is the whole path: the listing is read, the real
// published provenance is verified against the trust root compiled into this binary,
// the archive is downloaded and pinned to the digest that provenance names, the binary
// is extracted, it is made to prove it runs, and it replaces the running one.
func TestUpgradeInstallsAVerifiedRelease(t *testing.T) {
	newBinary := buildFakeFlynn(t, goldenTag)
	archive := packArchive(t, runtime.GOOS, newBinary)

	o := newOrigin(t)
	o.listRelease(goldenTag)

	// The provenance is the real one, so the digest the upgrade pins to is the digest
	// GitHub signed. This test therefore has to serve an archive whose digest matches
	// what the plan demands, which is exactly the constraint an attacker cannot meet.
	prov, err := release.Verify(goldenBundle(t))
	if err != nil {
		t.Fatalf("the golden bundle does not verify: %v", err)
	}
	asset := (&Updater{goos: runtime.GOOS, goarch: runtime.GOARCH}).assetName()
	if _, err := prov.Digest(asset); err != nil {
		t.Skipf("the golden release has no artifact for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	o.assets[goldenTag+"/flynn.intoto.jsonl"] = goldenBundle(t)
	o.assets[goldenTag+"/"+asset] = archive

	exe := installedFlynn(t, []byte("the old binary"))
	u := o.updater(t, "v0.1.2", exe)

	plan, err := u.Check(context.Background(), Request{AllowPrerelease: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Target.String() != goldenTag {
		t.Fatalf("target = %s, want %s", plan.Target, goldenTag)
	}
	if plan.Digest != prov.Artifacts[asset] {
		t.Fatalf("the plan pinned a digest the signed provenance does not name")
	}

	// The archive served here is not the one the real release published, so the pinned
	// digest will not match and the upgrade must refuse it. That is the single most
	// important assertion in this file: it is the case where an attacker has already
	// won everything except the signature.
	if err := u.Apply(context.Background(), plan); err == nil {
		t.Fatal("an archive whose digest does not match the signed provenance was installed")
	}
	if got, _ := os.ReadFile(exe); string(got) != "the old binary" {
		t.Fatal("a refused upgrade damaged the installed binary")
	}

	// Now let the plan pin the digest of the archive that is actually being served, which
	// is what a genuine release looks like from here, and the install must go through.
	plan.Digest = sha256hex(archive)
	if err := u.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := exec.Command(exe, "--version").Output()
	if err != nil {
		t.Fatalf("the installed binary does not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != goldenTag {
		t.Fatalf("the installed binary reports %q, want %s", strings.TrimSpace(string(got)), goldenTag)
	}

	// The floor rose, so the same version can never be walked back silently.
	if st := loadState(u.dataDir); st.HighestVerified != goldenTag {
		t.Errorf("highest verified = %q, want %s", st.HighestVerified, goldenTag)
	}
}

// A binary that downloads and verifies but does not run on this machine (the wrong
// libc, a broken build, a local policy that will not execute it) must not replace one
// that does.
func TestUpgradeKeepsTheOldBinaryWhenTheNewOneWillNotRun(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a compiler")
	}
	// A binary that is not a binary: it downloads fine, extracts fine, and will not run.
	archive := packArchive(t, runtime.GOOS, []byte("this is not an executable"))

	o := newOrigin(t)
	o.listRelease(goldenTag)
	o.assets[goldenTag+"/flynn.intoto.jsonl"] = goldenBundle(t)

	exe := installedFlynn(t, buildFakeFlynn(t, "v0.1.2"))
	u := o.updater(t, "v0.1.2", exe)

	asset := u.assetName()
	o.assets[goldenTag+"/"+asset] = archive

	plan, err := u.Check(context.Background(), Request{AllowPrerelease: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	plan.Digest = sha256hex(archive) // the download itself is honest; the binary is not

	if err := u.Apply(context.Background(), plan); err == nil {
		t.Fatal("a binary that does not run was installed over one that does")
	}

	// The old binary is still there, still working.
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		t.Fatalf("the original binary was damaged by a failed upgrade: %v", err)
	}
	if strings.TrimSpace(string(out)) != "v0.1.2" {
		t.Fatalf("the original binary reports %q", strings.TrimSpace(string(out)))
	}
	// And nothing was left behind in its directory.
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagedPrefix) {
			t.Errorf("a failed upgrade left %s behind", e.Name())
		}
	}
}

// The listing is the one unsigned input in the whole path. These are the attacks that
// live there, and none of them involve breaking a signature.
func TestListingAttacks(t *testing.T) {
	exe := installedFlynn(t, []byte("flynn"))

	t.Run("a downgrade to an old, genuinely signed release is refused", func(t *testing.T) {
		o := newOrigin(t)
		o.listRelease("v0.1.0")
		o.assets["v0.1.0/flynn.intoto.jsonl"] = goldenBundle(t)

		u := o.updater(t, "v0.9.0", exe)
		_, err := u.Check(context.Background(), Request{})
		if err == nil {
			t.Fatal("an older release was accepted as an upgrade")
		}
		if !strings.Contains(err.Error(), "older than") {
			t.Fatalf("err = %v", err)
		}
		if fault.Classify(err) != fault.Terminal {
			t.Errorf("class = %v, want terminal", fault.Classify(err))
		}
	})

	t.Run("a downgrade below a version already verified here is refused", func(t *testing.T) {
		o := newOrigin(t)
		o.listRelease("v0.1.1")
		u := o.updater(t, "v0.1.0", exe) // running an old one...
		// ...but this machine has already verified a newer one, so the listing offering
		// only the old one is either stale or hostile.
		if err := saveState(u.dataDir, state{HighestVerified: "v0.5.0"}); err != nil {
			t.Fatal(err)
		}
		if _, err := u.Check(context.Background(), Request{}); err == nil {
			t.Fatal("a version below the highest ever verified was accepted")
		}
	})

	t.Run("a listing that goes backwards is reported", func(t *testing.T) {
		o := newOrigin(t)
		o.listRelease("v0.1.0")
		o.assets["v0.1.0/flynn.intoto.jsonl"] = goldenBundle(t)

		u := o.updater(t, "v0.1.0", exe)
		if err := saveState(u.dataDir, state{NewestSeen: "v0.4.0"}); err != nil {
			t.Fatal(err)
		}
		plan, err := u.Check(context.Background(), Request{AllowDowngrade: true})
		// The provenance in the golden bundle is for a different tag, so the tag binding
		// fires first. Either way the upgrade does not happen; what this asserts is that
		// the freeze is noticed at all.
		if err == nil && plan.Warning == "" {
			t.Fatal("a listing that withdrew a release it had already offered went unremarked")
		}
		st := loadState(u.dataDir)
		if _, stale := st.staleListing(mustVersion(t, "v0.1.0")); !stale {
			t.Fatal("the state does not recognise a listing that went backwards")
		}
	})

	t.Run("a genuine bundle for a different release is refused", func(t *testing.T) {
		o := newOrigin(t)
		// The listing offers v0.9.9, but the bundle served for it is the real, valid,
		// correctly signed bundle for v0.1.3-rc.1. Every signature checks out. It is still
		// not the release that was asked for.
		o.listRelease("v0.9.9")
		o.assets["v0.9.9/flynn.intoto.jsonl"] = goldenBundle(t)

		u := o.updater(t, "v0.1.2", exe)
		_, err := u.Check(context.Background(), Request{})
		if err == nil {
			t.Fatal("a bundle from a different release was accepted")
		}
		if !strings.Contains(err.Error(), "provenance") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unsigned release is refused", func(t *testing.T) {
		o := newOrigin(t)
		o.listRelease("v0.9.9")
		o.assets["v0.9.9/flynn.intoto.jsonl"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)

		u := o.updater(t, "v0.1.2", exe)
		if _, err := u.Check(context.Background(), Request{}); err == nil {
			t.Fatal("a release with no valid provenance was accepted")
		}
	})

	t.Run("a prerelease is not offered as an upgrade unless asked for", func(t *testing.T) {
		o := newOrigin(t)
		o.listRelease("v0.2.0-rc.1", "v0.1.0")
		o.assets["v0.1.0/flynn.intoto.jsonl"] = goldenBundle(t)

		u := o.updater(t, "v0.1.0", exe)
		if _, err := u.Check(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "provenance") {
			// It picked v0.1.0 (the newest stable), not the rc: it got as far as checking
			// v0.1.0's provenance, which is the golden bundle for a different tag.
			t.Fatalf("the newest stable release was not chosen: %v", err)
		}
	})
}

// A source build has no release to be newer than, and upgrading it would throw away
// the developer's own binary.
func TestUpgradeRefusesToClobberASourceBuild(t *testing.T) {
	o := newOrigin(t)
	o.listRelease(goldenTag)
	u := o.updater(t, "0.0.0-dev", installedFlynn(t, []byte("flynn")))

	_, err := u.Check(context.Background(), Request{})
	if err == nil {
		t.Fatal("a source build tried to upgrade itself")
	}
	if !strings.Contains(err.Error(), "built from source") {
		t.Fatalf("err = %v", err)
	}
}

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, ok := ParseVersion(s)
	if !ok {
		t.Fatalf("%q does not parse", s)
	}
	return v
}
