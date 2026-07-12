package extension

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/sigstore"
)

// The release fixtures are the real signed artifacts from ionalpha/flynn-extensions
// token/v0.1.0: the same checksums.txt, signature and certificate a user downloads. The
// archive is rebuilt locally (a 3 MB binary is not worth vendoring), and the checksum file
// is re-signed for the rebuilt archive only where a test needs a *valid* release. Where a
// test needs the trust decision itself, it uses the real signature, because a signature
// this test suite produced would only prove the suite agrees with itself.
const (
	testWorkflow   = "https://github.com/ionalpha/go-ci/.github/workflows/monorepo-release.yml@refs/heads/main"
	testIssuer     = "https://token.actions.githubusercontent.com"
	testSourceRepo = "ionalpha/flynn-extensions"
)

func testOrigin() Origin {
	return Origin{
		Repo: testSourceRepo,
		Identity: sigstore.Identity{
			Workflow: testWorkflow, Issuer: testIssuer, SourceRepo: testSourceRepo,
		},
	}
}

// release is a fake GitHub release: a set of assets served over HTTP.
type release struct {
	assets map[string][]byte
	hits   map[string]int

	// ca signed this release, when the test built it. Nil means the fixtures are the
	// real, production-signed ones, which must be verified against the embedded Fulcio
	// roots like any user's download.
	ca *testCA
}

func newRelease() *release {
	return &release{assets: map[string][]byte{}, hits: map[string]int{}}
}

// serve stands up the release and returns a resolver pointed at it.
func (rel *release) serve(t *testing.T) (ReleaseResolver, *httptest.Server) {
	t.Helper()
	// TLS, because the downloader refuses plaintext: a release fetched over http could be
	// swapped in flight, and the check that would catch it is the one being tested.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path(r.URL.Path)
		rel.hits[name]++
		body, ok := rel.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r := ReleaseResolver{
		Origin:     testOrigin(),
		Dir:        t.TempDir(),
		Downloader: fetch.New(fetch.WithHTTPClient(srv.Client())),
		BaseURL:    srv.URL,
		GOOS:       "linux",
		GOARCH:     "amd64",
	}
	if rel.ca != nil {
		r.roots = rel.ca.rootPEM
	}
	return r, srv
}

// path reduces a request path to its asset name.
func path(p string) string {
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

// buildRelease produces a valid, self-consistent release: a tar.gz holding a binary, a
// checksums.txt covering it, and a signature over that file made by a forged CA whose root
// this test trusts. It exercises the resolver's plumbing (download, digest, extract, cache)
// without needing the real 3 MB archive.
//
// It cannot be used to prove the trust decision, because the signature is one the test
// made. TestRefusesTheRealReleaseUnderTheWrongPin does that, against the real signature.
func buildRelease(t *testing.T, binBody string) *release {
	t.Helper()
	rel := newRelease()
	rel.ca = newTestCA(t)

	archive := tarGz(t, "token", binBody)
	name := "token_v0.1.0_linux_amd64.tar.gz"
	rel.assets[name] = archive

	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	rel.assets["checksums.txt"] = checksums

	certPEM, sig := rel.ca.sign(t, checksums, testWorkflow, testIssuer, testSourceRepo)
	rel.assets["checksums.txt.sig"] = sig
	rel.assets["checksums.txt.pem"] = certPEM
	return rel
}

// tarGz builds a tar.gz containing one executable file.
func tarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

func releaseBlock() ProcessBlock {
	return ProcessBlock{Release: &ReleaseSource{Asset: "token", Version: "v0.1.0"}}
}

// TestResolvesAndInstallsAVerifiedRelease drives the happy path: a signed release is
// downloaded, verified, extracted, and its binary handed back to be run.
func TestResolvesAndInstallsAVerifiedRelease(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	r, _ := rel.serve(t)

	bin, args, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err != nil {
		t.Fatalf("a correctly signed release did not resolve: %v", err)
	}
	if args != nil {
		t.Errorf("args = %v, want nil", args)
	}
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "echo token") {
		t.Errorf("resolved binary is not the one in the archive")
	}
	// A receipt records what was proven, so a later run can re-check it.
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), verifiedFile)); err != nil {
		t.Errorf("no verification receipt was written: %v", err)
	}
}

// TestTamperedArchiveIsRefused is the acceptance criterion: the signature covers the
// checksum file, the checksum file covers the archive, so an archive swapped on the server
// cannot be run even though it is served from the real release URL under the real name.
func TestTamperedArchiveIsRefused(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	// The attacker replaces the binary but cannot re-sign the checksum file.
	rel.assets["token_v0.1.0_linux_amd64.tar.gz"] = tarGz(t, "token", "#!/bin/sh\necho pwned\n")

	r, _ := rel.serve(t)

	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err == nil {
		t.Fatal("a tampered archive was accepted and would have been executed")
	}
	// It fails at the digest check inside the download, which is what the signature
	// bought us: the pinned digest came from a file we proved was signed.
	if !strings.Contains(strings.ToLower(err.Error()), "digest") &&
		!strings.Contains(strings.ToLower(err.Error()), "sha256") &&
		!strings.Contains(strings.ToLower(err.Error()), "mismatch") {
		t.Errorf("refused, but not for the right reason: %v", err)
	}
}

// TestUnsignedReleaseIsRefused: the archive and checksums are consistent, but nobody signed
// them. Consistency is not provenance.
func TestUnsignedReleaseIsRefused(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	rel.assets["checksums.txt.sig"] = []byte("dGhpcyBpcyBub3QgYSBzaWduYXR1cmU=")

	r, _ := rel.serve(t)

	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	assertCode(t, err, "extension_release_unverified")
}

// TestRefusesAnArchiveTheSignatureDoesNotCover: the signature is genuine and the checksum
// file is genuine, but this platform's archive is not listed in it. A file served under a
// plausible name that the signed manifest never mentions is not part of the release.
func TestRefusesAnArchiveTheSignatureDoesNotCover(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	r, _ := rel.serve(t)
	// Resolve for a platform the signed checksum file says nothing about.
	r.GOARCH = "arm64"
	rel.assets["token_v0.1.0_linux_arm64.tar.gz"] = tarGz(t, "token", "#!/bin/sh\necho smuggled\n")

	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	assertCode(t, err, "extension_release_not_in_manifest")
}

// TestRefusesTheRealReleaseUnderTheWrongPin uses the real, production signature from
// token/v0.1.0 and checks the resolver's trust decision against it: signed by the actual
// workflow, but the operator pinned a different one. This is the substitution a resolver
// that merely checks "is there a valid signature" would wave through.
func TestRefusesTheRealReleaseUnderTheWrongPin(t *testing.T) {
	t.Parallel()
	rel := newRelease()
	rel.assets["checksums.txt"] = realAsset(t, "checksums.txt")
	rel.assets["checksums.txt.sig"] = realAsset(t, "checksums.txt.sig")
	rel.assets["checksums.txt.pem"] = realAsset(t, "checksums.txt.pem")

	r, _ := rel.serve(t)
	// Everything real, except that we trust a different workflow to have built it.
	r.Origin.Identity.Workflow = "https://github.com/attacker/evil/.github/workflows/release.yml@refs/heads/main"

	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	assertCode(t, err, "extension_release_unverified")
}

// TestAcceptsTheRealReleaseSignature is the counterpart: the same real artifacts, under the
// correct pin, pass verification. Together the two prove the pin is what decides, and that
// the production release actually satisfies it.
func TestAcceptsTheRealReleaseSignature(t *testing.T) {
	t.Parallel()
	if err := sigstore.Verify(
		realAsset(t, "checksums.txt"),
		realAsset(t, "checksums.txt.sig"),
		realAsset(t, "checksums.txt.pem"),
		DefaultOrigin.Identity,
	); err != nil {
		t.Fatalf("the shipped DefaultOrigin does not verify the real token/v0.1.0 release: %v", err)
	}
}

// TestCachedInstallIsReused: a second resolve of the same version does not re-download.
func TestCachedInstallIsReused(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	r, _ := rel.serve(t)

	first, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err != nil {
		t.Fatal(err)
	}
	downloads := rel.hits["token_v0.1.0_linux_amd64.tar.gz"]

	second, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("cached resolve returned a different path: %q vs %q", first, second)
	}
	if got := rel.hits["token_v0.1.0_linux_amd64.tar.gz"]; got != downloads {
		t.Errorf("the archive was downloaded again on a cached resolve (%d -> %d)", downloads, got)
	}
}

// TestTamperedCacheIsRefused: an attacker who cannot break the signature can still try to
// overwrite the binary after it was verified. The receipt is what catches that, which is
// why a cached install is re-hashed rather than trusted for being on disk.
func TestTamperedCacheIsRefused(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	r, _ := rel.serve(t)

	bin, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err = r.Resolve(context.Background(), "token", releaseBlock())
	assertCode(t, err, "extension_release_cache_tampered")
}

// TestRefusesWithoutAPinnedOrigin: a resolver that was never told who to trust must refuse
// everything, not trust anyone.
func TestRefusesWithoutAPinnedOrigin(t *testing.T) {
	t.Parallel()
	r := ReleaseResolver{Dir: t.TempDir(), Downloader: fetch.New()}
	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	assertCode(t, err, "extension_release_no_origin")
}

// TestRefusesADevSource: the release resolver must never honour an unsigned local path,
// or the signed-distribution guarantee could be sidestepped by a spec that declares both.
func TestRefusesADevSource(t *testing.T) {
	t.Parallel()
	r := ReleaseResolver{Origin: testOrigin(), Dir: t.TempDir(), Downloader: fetch.New()}
	_, _, err := r.Resolve(context.Background(), "token", ProcessBlock{
		Dev: &DevSource{Path: filepath.Join(t.TempDir(), "token")},
	})
	assertCode(t, err, "extension_release_absent")
}

// TestArchiveNameMatchesThePublishedContract pins the asset name against the names the
// release tooling actually publishes. These two strings are one contract; if they drift,
// every install 404s.
func TestArchiveNameMatchesThePublishedContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "token_v0.1.0_linux_amd64.tar.gz"},
		{"darwin", "arm64", "token_v0.1.0_darwin_arm64.tar.gz"},
		{"windows", "amd64", "token_v0.1.0_windows_amd64.zip"},
	} {
		got := archiveName("token", "v0.1.0", target{tc.goos, tc.goarch})
		if got != tc.want {
			t.Errorf("archiveName(%s/%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}

	// And the URL, whose tag segment contains a slash. GitHub serves it unchanged; that
	// is what makes a per-extension tag usable as a download path at all.
	r := ReleaseResolver{Origin: DefaultOrigin}
	want := "https://github.com/ionalpha/flynn-extensions/releases/download/token/v0.1.0/token_v0.1.0_linux_amd64.tar.gz"
	if got := r.assetURL(ReleaseSource{Asset: "token", Version: "v0.1.0"}, "token_v0.1.0_linux_amd64.tar.gz"); got != want {
		t.Errorf("assetURL = %q, want %q", got, want)
	}
}

func realAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "internal", "sigstore", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected refusal with %s, but it was accepted", want)
	}
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != want {
		t.Fatalf("refused with %v, want code %s", err, want)
	}
}

// pinnedBlock is releaseBlock with an explicit archive digest for the running platform.
func pinnedBlock(t *testing.T, digest string) ProcessBlock {
	t.Helper()
	b := releaseBlock()
	// The fixture resolver serves linux/amd64 (see serve), so pin that platform.
	b.Release.Digests = map[string]string{"linux_amd64": digest}
	return b
}

// TestRecutTagIsRefusedEvenWhenCorrectlySigned is the reason the digest pin exists.
//
// A git tag is mutable. Whoever can write to the extensions repo can delete a released tag,
// re-cut it against different code, and publish it through the very same trusted release
// workflow. The new artifact then carries a perfectly valid signature from the pinned
// identity, because the signature only ever proved "the pinned workflow built this" - never
// "this is the artifact that was reviewed". Signature verification alone therefore cannot
// stop the substitution; every flynn in the world would download the new binary and run it.
//
// The pinned digest can, because it is compiled into the flynn binary before the attack
// exists. Here the served release is entirely legitimate and correctly signed; it simply is
// not the artifact this flynn pins, and that is enough to refuse it.
func TestRecutTagIsRefusedEvenWhenCorrectlySigned(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho attacker\n")
	r, _ := rel.serve(t)

	_, _, err := r.Resolve(context.Background(), "token", pinnedBlock(t, strings.Repeat("ab", 32)))
	if err == nil {
		t.Fatal("a re-cut tag was accepted because its signature verified: the pin must be on the BYTES, not on the version string")
	}
	if !strings.Contains(err.Error(), "digest_mismatch") && !strings.Contains(err.Error(), "NOT the artifact") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestPinnedDigestThatMatchesResolves proves the pin does not break the legitimate release:
// the artifact whose digest the spec names is installed and run as normal.
func TestPinnedDigestThatMatchesResolves(t *testing.T) {
	t.Parallel()
	rel := buildRelease(t, "#!/bin/sh\necho token\n")
	r, _ := rel.serve(t)

	// Learn the real digest the way the resolver does, then pin exactly that.
	unpinned, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err != nil {
		t.Fatalf("baseline resolve failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(unpinned), verifiedFile))
	if err != nil {
		t.Fatal(err)
	}
	var v verified
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}

	// A fresh resolver (fresh install dir) so the pin is exercised on the download path.
	r2, _ := rel.serve(t)
	if _, _, err := r2.Resolve(context.Background(), "token", pinnedBlock(t, v.ArchiveSHA256)); err != nil {
		t.Fatalf("the correctly pinned release was refused: %v", err)
	}
}
