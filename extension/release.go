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
	"github.com/ionalpha/flynn/internal/sigstore"
)

// Origin describes where released extensions come from and who is trusted to have built
// them. It is the trust root for every extension flynn will execute: change these fields
// and flynn runs somebody else's code.
type Origin struct {
	// Repo is the "owner/name" whose releases are installed from.
	Repo string

	// Identity is the signature the release must carry. Note that Identity.Workflow is
	// the *reusable* release workflow, which lives in a different repository than Repo;
	// that is what Sigstore binds the signature to, and pinning Repo instead would
	// reject every genuine release.
	Identity sigstore.Identity
}

// DefaultOrigin is the first-party extension catalog: extensions published by
// ionalpha/flynn-extensions, signed by the shared release workflow that builds them.
//
// It is a var rather than a const so a test can point the resolver at a fixture server,
// and so an operator running a private catalog can replace it wholesale. It is not a
// per-extension setting: an extension does not get to say who vouches for it.
var DefaultOrigin = Origin{
	Repo: "ionalpha/flynn-extensions",
	Identity: sigstore.Identity{
		Workflow:   "https://github.com/ionalpha/go-ci/.github/workflows/monorepo-release.yml@refs/heads/main",
		Issuer:     "https://token.actions.githubusercontent.com",
		SourceRepo: "ionalpha/flynn-extensions",
	},
}

// maxMetadataBytes caps the checksum file, signature and certificate. They are a few
// kilobytes; anything remotely larger is not the file we asked for, and a resolver that
// will read whatever a server sends is a resolver that can be made to exhaust memory.
const maxMetadataBytes = 1 << 20 // 1 MiB

// maxArchiveBytes caps an extension archive. Extensions are single static binaries.
const maxArchiveBytes = 256 << 20 // 256 MiB

// verifiedFile records what was proven about an installed extension, so a cached install
// can be re-checked without going back to the network.
const verifiedFile = ".flynn-verified.json"

// verified is the receipt written beside an installed extension.
type verified struct {
	Archive       string `json:"archive"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
	Workflow      string `json:"workflow"`
	SourceRepo    string `json:"source_repo"`
}

// ReleaseResolver resolves a released extension to a local binary it has proven the origin
// of, and refuses to produce a path for anything it cannot prove.
//
// The chain of trust runs: a keyless signature over the release's checksums.txt, made by
// the pinned workflow identity; that file commits to every archive by SHA-256; the archive
// downloaded for this platform must match its digest; the binary extracted from it is
// hashed and the hash recorded. Nothing is executed on the strength of where it was
// downloaded from, only on the strength of what signed it.
type ReleaseResolver struct {
	// Origin is who is trusted. The zero value trusts nobody and refuses everything.
	Origin Origin

	// Dir is where verified extensions are installed, one directory per name and version.
	Dir string

	// Downloader performs the size-capped, digest-checked downloads.
	Downloader *fetch.Downloader

	// BaseURL overrides the release host (tests point this at a local server). Empty
	// means GitHub.
	BaseURL string

	// GOOS and GOARCH override the target platform. Empty means this machine's.
	GOOS, GOARCH string

	// roots overrides the Sigstore trust anchor. It is unexported because only this
	// package's tests set it: production must always pin the embedded Fulcio roots, and
	// an exported knob for "trust a different CA" is one an operator could get wrong.
	roots []byte
}

// Resolve downloads, verifies and installs the extension named by the block's release
// source, returning the path to its binary. A dev source is not this resolver's business
// and is refused: honouring one here would let an unsigned local path stand in for a
// signed release, which is the one substitution the whole design exists to prevent.
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

	// A cached install is re-proven from its receipt rather than trusted because it is
	// there: the directory is on local disk, and local disk is exactly what an attacker
	// who already got a foothold would rewrite.
	if bin, ok, err := r.reuse(targetDir, archive, plat.binaryName(rel.Asset), rel.Digests[plat.goos+"_"+plat.goarch]); err != nil {
		return "", nil, err
	} else if ok {
		return bin, block.Args, nil
	}

	digest, err := r.provenDigest(ctx, extName, rel, archive)
	if err != nil {
		return "", nil, err
	}

	// The signature proves the pinned workflow built this release. It does NOT prove the
	// release is the one this flynn was built to run: a mutable tag, re-cut and re-published
	// through the same workflow, produces a different binary with a perfectly valid
	// signature. So if the spec pinned a digest, that digest is the last word. This check
	// runs against a hash compiled into the binary, which is why an attacker who owns the
	// repo, the tag, and the release pipeline still cannot change what an already-shipped
	// flynn executes.
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

// provenDigest establishes the archive's digest from the release's signature. The digest
// is never taken from the artifact itself, or from any file the artifact's publisher could
// have written without signing: it comes from checksums.txt, and checksums.txt is trusted
// only after the pinned identity is proven to have signed it.
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
		// Deliberately not wrapped into something vaguer: the operator needs to know
		// that code claiming to be this extension failed to prove who built it.
		return "", fault.Wrap(fault.Forbidden, "extension_release_unverified",
			fmt.Errorf("extension %s: refusing to run code whose origin is not proven: %w", extName, err))
	}

	digest, ok := digestFor(checksums, archive)
	if !ok {
		// The signature covers the file, and the file does not mention this archive. So
		// the archive is not part of the release that was signed, whatever the server
		// serves under its name.
		return "", fault.New(fault.Forbidden, "extension_release_not_in_manifest",
			"extension: "+extName+" release "+rel.Version+" does not include "+archive+
				"; the signed checksum file does not cover it, so it is not part of this release")
	}
	return digest, nil
}

// reuse re-proves an already-installed extension from its receipt. A missing receipt, a
// receipt for a different archive or origin, a missing binary, or a binary whose hash has
// moved all mean "install again and re-verify" rather than "run it".
func (r ReleaseResolver) reuse(targetDir, archive, binName, wantArchive string) (string, bool, error) {
	// A missing or unreadable receipt is not an error: it means this version was never
	// installed, or was installed by a flynn that wrote no receipt. Either way the answer
	// is to download and verify it again, which is the safe direction. Reporting an error
	// here would instead break an install that is merely unproven.
	raw, err := os.ReadFile(filepath.Join(targetDir, verifiedFile)) //nolint:gosec // G304: a path under our own data dir
	if err != nil {
		return "", false, nil //nolint:nilerr // no receipt means "verify again", not "fail"
	}
	var v verified
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, nil //nolint:nilerr // a corrupt receipt proves nothing, so re-verify
	}
	if v.Archive != archive || v.Workflow != r.Origin.Identity.Workflow || v.SourceRepo != r.Origin.Identity.SourceRepo {
		// The pinned origin has changed since this was installed, so what was proven
		// then is not what we require now.
		return "", false, nil
	}
	// If the spec pins a digest, the cached install must be OF that digest. Otherwise the
	// cache is the bypass: an install made when the tag pointed at other bytes would be
	// reused forever, and the pin would only ever apply to machines that had never run the
	// extension before. Re-download and let the digest check above reject it properly.
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
		// The cached binary is not the one that was verified. Something rewrote it on
		// disk, which is the only way this happens.
		return "", false, fault.New(fault.Forbidden, "extension_release_cache_tampered",
			"extension: the installed binary at "+targetDir+" no longer matches the digest that was verified; refusing to run it")
	}
	return bin, true, nil
}

func (r ReleaseResolver) writeReceipt(dir string, v verified) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, "extension_release_receipt", err)
	}
	if err := os.WriteFile(filepath.Join(dir, verifiedFile), append(raw, '\n'), 0o600); err != nil {
		return fault.Wrap(fault.Terminal, "extension_release_receipt", err)
	}
	return nil
}

// fetchBytes downloads a small metadata file and returns its contents. No digest is pinned
// because there is nothing yet to pin against: these three files are what establish the
// pin. They are size-capped instead, and the signature is what makes them trustworthy.
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

// archiveName is the release asset a given platform downloads. It mirrors, exactly, the
// name the release tooling publishes; the two are one contract, and a mismatch here is a
// 404 rather than a security hole, because the digest is what is trusted.
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

// assetURL is where a release asset lives. The tag is "<asset>/<version>", so the path
// carries a slash inside the tag; GitHub serves it unchanged, which is why the release
// tooling can use a prefixed tag at all.
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

// SourceResolver routes an extension to the resolver its source kind demands: a published
// release goes through signature verification, a dev binary through the explicit dev-mode
// opt-in.
//
// A release wins whenever both are declared. That is the point of the ordering: a spec
// carrying a stray dev block, whether from an author's machine or from an attacker who
// managed to edit a stored spec, must not be able to substitute an unsigned local binary
// for the signed release the operator asked for. Downgrade is the attack; refusing to
// downgrade is the defence.
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
