package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/sigstore"
)

// Origin is the trust root for released extensions: where they come from and
// who must have built them.
type Origin struct {
	// Repo is "owner/name".
	Repo string

	// Identity.Workflow is the reusable release workflow, in a different
	// repository from Repo. Sigstore binds the signature to it, so pinning
	// Repo would reject every genuine release.
	Identity sigstore.Identity
}

// DefaultOrigin is the first-party catalog, ionalpha/flynn-extensions. It is a
// var so tests and private catalogs can replace it. It is never per-extension:
// an extension does not choose who vouches for it.
var DefaultOrigin = Origin{
	Repo: "ionalpha/flynn-extensions",
	Identity: sigstore.Identity{
		Workflow:   "https://github.com/ionalpha/go-ci/.github/workflows/monorepo-release.yml@refs/heads/main",
		Issuer:     "https://token.actions.githubusercontent.com",
		SourceRepo: "ionalpha/flynn-extensions",
	},
}

// maxMetadataBytes caps the checksum file, signature and certificate, which
// are a few kilobytes, so a server cannot exhaust memory.
const maxMetadataBytes = 1 << 20 // 1 MiB

// maxArchiveBytes caps an extension archive. Extensions are single static binaries.
const maxArchiveBytes = 256 << 20 // 256 MiB

// verifiedFile lets a cached install be re-checked offline.
const verifiedFile = ".flynn-verified.json"

// verified is the receipt written beside an installed extension.
type verified struct {
	Archive       string `json:"archive"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
	Workflow      string `json:"workflow"`
	SourceRepo    string `json:"source_repo"`
}

// ReleaseResolver returns a binary only when it can prove its origin. The chain:
// the pinned workflow signs checksums.txt, which commits to each archive by
// SHA-256; the downloaded archive must match; the extracted binary's hash is
// recorded. Download location confers no trust.
type ReleaseResolver struct {
	// Origin's zero value trusts nobody.
	Origin Origin

	// Dir holds one directory per name and version.
	Dir string

	Downloader *fetch.Downloader

	// BaseURL overrides the release host; empty means GitHub.
	BaseURL string

	// GOOS and GOARCH default to this machine's.
	GOOS, GOARCH string

	// roots overrides the Sigstore trust anchor for tests only; production
	// always pins the embedded Fulcio roots.
	roots []byte
}

// Resolve downloads, verifies and installs a release source and returns the
// binary path. A dev source is refused so an unsigned path cannot stand in
// for a signed release.
func (r ReleaseResolver) Resolve(ctx context.Context, extName string, block ProcessBlock) (string, []string, error) {
	if block.Release == nil {
		return "", nil, fault.New(fault.Terminal, "extension_release_absent",
			"extension: "+extName+" declares no released source; the release resolver runs only signed, published extensions")
	}
	rel := *block.Release
	if rel.Asset == "" || rel.Version == "" {
		return "", nil, fault.New(fault.Terminal, "extension_release_incomplete",
			"extension: "+extName+" release source must name both an asset and a version")
	}
	if r.Origin.Repo == "" || r.Origin.Identity.Workflow == "" {
		return "", nil, fault.New(fault.Terminal, "extension_release_no_origin",
			"extension: no release origin is pinned, so nothing can be trusted to have built "+extName)
	}
	if r.Downloader == nil {
		return "", nil, fault.New(fault.Terminal, "extension_release_no_downloader",
			"extension: release resolver has no downloader")
	}

	plat := r.platform()
	archive := archiveName(rel.Asset, rel.Version, plat)
	targetDir := filepath.Join(r.Dir, rel.Asset, rel.Version, plat.goos+"_"+plat.goarch)

	// A cached install is verified again: local disk is what an attacker with a
	// foothold rewrites.
	if bin, ok, err := r.reuse(targetDir, archive, plat.binaryName(rel.Asset), rel.Digests[plat.goos+"_"+plat.goarch]); err != nil {
		return "", nil, err
	} else if ok {
		return bin, block.Args, nil
	}

	digest, err := r.provenDigest(ctx, extName, rel, archive)
	if err != nil {
		return "", nil, err
	}

	// A re-cut tag through the same workflow carries a valid signature over
	// different bytes, so a pinned digest has the last word. It is compiled
	// in, so owning the repo and pipeline cannot change what a shipped flynn
	// runs.
	if want, ok := rel.Digests[plat.goos+"_"+plat.goarch]; ok && !strings.EqualFold(want, digest) {
		return "", nil, fault.New(fault.Forbidden, "extension_release_digest_mismatch",
			"extension: "+extName+" release "+rel.Version+" is correctly signed but is NOT the artifact this flynn pins: "+
				"expected archive sha256 "+want+", got "+digest+". The tag has been re-cut, or the release was rebuilt; refusing to run it")
	}

	binPath, _, err := acquire.InstallTo(ctx, r.Downloader, acquire.Release{
		URL:       r.assetURL(rel, archive),
		SHA256:    digest,
		SizeBytes: maxArchiveBytes,
		Archive:   plat.archiveKind(),
		BinName:   plat.binaryName(rel.Asset),
	}, targetDir)
	if err != nil {
		return "", nil, fault.Wrap(fault.Terminal, "extension_release_install", err)
	}

	binHash, err := hashFile(binPath)
	if err != nil {
		return "", nil, err
	}
	if err := r.writeReceipt(targetDir, verified{
		Archive:       archive,
		ArchiveSHA256: digest,
		BinarySHA256:  binHash,
		Workflow:      r.Origin.Identity.Workflow,
		SourceRepo:    r.Origin.Identity.SourceRepo,
	}); err != nil {
		return "", nil, err
	}
	return binPath, block.Args, nil
}

// provenDigest reads the archive's digest from checksums.txt, trusted only
// once the pinned identity's signature over it verifies.
func (r ReleaseResolver) provenDigest(ctx context.Context, extName string, rel ReleaseSource, archive string) (string, error) {
	tmp, err := os.MkdirTemp("", "flynn-ext-verify-")
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "extension_release_tmp", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var parts [3][]byte
	for i, name := range []string{"checksums.txt", "checksums.txt.sig", "checksums.txt.pem"} {
		b, err := r.fetchBytes(ctx, r.metadataURL(rel, name), filepath.Join(tmp, name))
		if err != nil {
			return "", fault.Wrap(fault.Terminal, "extension_release_metadata", err)
		}
		parts[i] = b
	}
	checksums, sig, cert := parts[0], parts[1], parts[2]

	if err := (sigstore.Verifier{Roots: r.roots}).Verify(checksums, sig, cert, r.Origin.Identity); err != nil {
		// Unwrapped: the operator must see that the build provenance failed.
		return "", fault.Wrap(fault.Forbidden, "extension_release_unverified",
			fmt.Errorf("extension %s: refusing to run code whose origin is not proven: %w", extName, err))
	}

	digest, ok := digestFor(checksums, archive)
	if !ok {
		// An archive the signed file does not list is not part of the release.
		return "", fault.New(fault.Forbidden, "extension_release_not_in_manifest",
			"extension: "+extName+" release "+rel.Version+" does not include "+archive+
				"; the signed checksum file does not cover it, so it is not part of this release")
	}
	return digest, nil
}

// reuse re-proves an install from its receipt. Any mismatch means reinstall.
func (r ReleaseResolver) reuse(targetDir, archive, binName, wantArchive string) (string, bool, error) {
	// An unreadable receipt means unproven, so reinstall instead of failing.
	raw, err := os.ReadFile(filepath.Join(targetDir, verifiedFile)) //nolint:gosec // G304: a path under our own data dir
	if err != nil {
		return "", false, nil //nolint:nilerr // no receipt means "verify again", not "fail"
	}
	var v verified
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, nil //nolint:nilerr // a corrupt receipt proves nothing, so re-verify
	}
	if v.Archive != archive || v.Workflow != r.Origin.Identity.Workflow || v.SourceRepo != r.Origin.Identity.SourceRepo {
		// The pinned origin changed since install.
		return "", false, nil
	}
	// A cached install must match a pinned digest, or one made when the tag
	// pointed elsewhere would bypass the pin forever.
	if wantArchive != "" && !strings.EqualFold(v.ArchiveSHA256, wantArchive) {
		return "", false, nil
	}
	bin, ok := acquire.FindBinary(targetDir, binName)
	if !ok {
		return "", false, nil
	}
	got, err := hashFile(bin)
	if err != nil {
		return "", false, err
	}
	if got != v.BinarySHA256 {
		// Something rewrote the binary on disk.
		return "", false, fault.New(fault.Forbidden, "extension_release_cache_tampered",
			"extension: the installed binary at "+targetDir+" no longer matches the digest that was verified; refusing to run it")
	}
	return bin, true, nil
}

// writeReceipt writes atomically: a torn receipt would fail a good install.
func (r ReleaseResolver) writeReceipt(dir string, v verified) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, "extension_release_receipt", err)
	}
	if err := fsatomic.WriteFile(filepath.Join(dir, verifiedFile), append(raw, '\n'), 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "extension_release_receipt", err)
	}
	return nil
}

// fetchBytes downloads a metadata file, size-capped. These files establish
// the pin, so none is digest-checked; the signature vouches for them.
func (r ReleaseResolver) fetchBytes(ctx context.Context, url, dest string) ([]byte, error) {
	if _, err := r.Downloader.Fetch(ctx, fetch.Request{
		URL:      url,
		Dest:     dest,
		MaxBytes: maxMetadataBytes,
	}); err != nil {
		return nil, err
	}
	return os.ReadFile(dest) //nolint:gosec // G304: a temp path this function just created
}

// digestFor finds an archive's SHA-256 in a `sha256sum`-format checksum file.
func digestFor(checksums []byte, archive string) (string, bool) {
	for line := range strings.SplitSeq(strings.TrimSpace(string(checksums)), "\n") {
		digest, name, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == archive {
			return digest, true
		}
	}
	return "", false
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: hashing a binary we resolved and are about to run
	if err != nil {
		return "", fault.Wrap(fault.Terminal, "extension_release_hash", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fault.Wrap(fault.Terminal, "extension_release_hash", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// target is the platform an extension is resolved for.
type target struct{ goos, goarch string }

func (r ReleaseResolver) platform() target {
	t := target{goos: r.GOOS, goarch: r.GOARCH}
	if t.goos == "" {
		t.goos = runtime.GOOS
	}
	if t.goarch == "" {
		t.goarch = runtime.GOARCH
	}
	return t
}

func (t target) archiveKind() acquire.ArchiveKind {
	if t.goos == "windows" {
		return acquire.ArchiveZip
	}
	return acquire.ArchiveTarGz
}

func (t target) binaryName(asset string) string {
	if t.goos == "windows" {
		return asset + ".exe"
	}
	return asset
}

// archiveName must match what the release tooling publishes. A mismatch is a
// 404 and never a trust failure, since the digest is what is trusted.
//
//	token_v0.1.0_linux_amd64.tar.gz
//	token_v0.1.0_windows_amd64.zip
func archiveName(asset, version string, t target) string {
	ext := ".tar.gz"
	if t.goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", asset, version, t.goos, t.goarch, ext)
}

// assetURL builds the download URL. The tag "<asset>/<version>" contains a
// slash, which GitHub serves unchanged.
func (r ReleaseResolver) assetURL(rel ReleaseSource, name string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s/%s",
		r.baseURL(), r.Origin.Repo, rel.Asset, rel.Version, name)
}

func (r ReleaseResolver) metadataURL(rel ReleaseSource, name string) string {
	return r.assetURL(rel, name)
}

func (r ReleaseResolver) baseURL() string {
	if r.BaseURL != "" {
		return strings.TrimSuffix(r.BaseURL, "/")
	}
	return "https://github.com"
}

var _ Resolver = ReleaseResolver{}

// SourceResolver routes a release to signature verification and a dev binary
// to the dev-mode opt-in. A release wins when both are declared, so a stray
// or planted dev block cannot downgrade a signed release to an unsigned
// binary.
type SourceResolver struct {
	Release ReleaseResolver
	Dev     DevResolver
}

// Resolve dispatches on the source the spec declares.
func (s SourceResolver) Resolve(ctx context.Context, extName string, block ProcessBlock) (string, []string, error) {
	if block.Release != nil {
		return s.Release.Resolve(ctx, extName, block)
	}
	if block.Dev != nil {
		return s.Dev.Resolve(ctx, extName, block)
	}
	return "", nil, fault.New(fault.Terminal, "extension_no_source",
		"extension: "+extName+" names neither a released nor a dev binary, so there is nothing to run")
}

var _ Resolver = SourceResolver{}
