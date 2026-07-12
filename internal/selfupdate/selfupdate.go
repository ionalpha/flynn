// Package selfupdate lets an installed flynn replace itself with a newer one, without
// a shell script, a package manager, or any tool the user does not already have.
//
// The security of an update path is not in how it downloads: it is in what it refuses
// to install. Everything this package fetches is treated as hostile until the signed
// provenance in internal/release says otherwise, and the provenance is what the
// download is then pinned to. A compromised mirror, a hostile proxy, a certificate
// authority in the wrong hands, or a GitHub outage substituting the wrong bytes all
// end the same way: a digest that does not match, and nothing written.
//
// The release listing is the one input that is not signed, and it is used only to
// enumerate candidates, never as evidence. An attacker who controls it can hide new
// versions or offer old ones, so this package remembers the highest version it has
// ever verified and the newest it has ever been offered, and refuses to move backwards
// past either without being asked in as many words.
//
// The install is the other half, and it is where update mechanisms usually break: the
// new binary is staged in the same directory as the one it replaces so the swap is a
// rename within one filesystem, the running binary's path is resolved through its
// symlinks so the write lands where it was aimed, an install owned by a package
// manager is refused rather than trampled, and the new binary is made to prove it runs
// before it is kept. Nothing that has not verified is ever executed.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/release"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/sandbox"
)

// Failure codes.
const (
	CodeListing    = "selfupdate.listing"
	CodeNoRelease  = "selfupdate.no_release"
	CodeDowngrade  = "selfupdate.downgrade"
	CodeStale      = "selfupdate.stale_listing"
	CodeArchive    = "selfupdate.archive"
	CodeInstall    = "selfupdate.install"
	CodeManaged    = "selfupdate.managed_install"
	CodePermission = "selfupdate.permission"
	CodeState      = "selfupdate.state"
	CodeSmokeTest  = "selfupdate.smoke_test"
	CodeDevBuild   = "selfupdate.dev_build"
)

const (
	// listingURL enumerates candidate releases. Nothing it says is trusted.
	listingURL = "https://api.github.com/repos/ionalpha/flynn/releases?per_page=100"
	// downloadBase is where a release's assets live.
	downloadBase = "https://github.com/ionalpha/flynn/releases/download/"
	// bundleAsset is the signed provenance attached to every release.
	bundleAsset = "flynn.intoto.jsonl"

	maxListingBytes = 4 << 20
	maxBundleBytes  = 4 << 20
	maxArchiveBytes = 512 << 20

	listingTimeout = 30 * time.Second
	// smokeTestTimeout bounds the one execution of the new binary. Printing a version
	// is instant; a binary that cannot manage it in this long is not one to install.
	smokeTestTimeout = 30 * time.Second
)

// Updater upgrades the running binary.
type Updater struct {
	dataDir string
	http    *http.Client
	fetcher *fetch.Downloader
	clock   clock.Clock

	// These are injected so a test can run the whole path, including the install, in a
	// temporary directory against a local server, rather than only testing the parts
	// that do not touch the disk. The parts that touch the disk are the risky ones.
	listingURL   string
	downloadBase string
	exe          func() (string, error)
	goos, goarch string
	version      string
}

// Option configures an Updater.
type Option func(*Updater)

// New builds an Updater that keeps its state under dataDir.
func New(dataDir string, opts ...Option) *Updater {
	u := &Updater{
		dataDir:      dataDir,
		clock:        clock.System{},
		listingURL:   listingURL,
		downloadBase: downloadBase,
		exe:          os.Executable,
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
	}
	for _, o := range opts {
		o(u)
	}
	if u.http == nil {
		u.http = &http.Client{Timeout: listingTimeout}
	}
	if u.fetcher == nil {
		u.fetcher = fetch.New()
	}
	return u
}

// Release is one release the listing offered. It is unverified: the tag is a candidate
// to go and check, not a fact.
type Release struct {
	Version     Version
	Prerelease  bool
	PublishedAt time.Time
	// Current marks the release this binary is running.
	Current bool
}

// List reports the releases that exist, newest first. It verifies nothing, because
// there is nothing here to verify: the listing exists to answer "what should I go and
// check", and every answer it gives is checked before a byte of it is installed.
func (u *Updater) List(ctx context.Context) ([]Release, error) {
	raw, err := u.get(ctx, u.listingURL, maxListingBytes)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		TagName     string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fault.Wrap(fault.Transient, CodeListing, fmt.Errorf("reading the release listing: %w", err))
	}

	current, _ := ParseVersion(u.current())
	out := make([]Release, 0, len(entries))
	for _, e := range entries {
		v, ok := ParseVersion(e.TagName)
		// A tag that is not a version is not a release this binary knows how to be.
		if !ok || e.Draft {
			continue
		}
		out = append(out, Release{
			Version:     v,
			Prerelease:  e.Prerelease || v.IsPrerelease(),
			PublishedAt: e.PublishedAt,
			Current:     current.raw != "" && v.Compare(current) == 0,
		})
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Transient, CodeNoRelease, "the release listing offered no releases")
	}
	sortReleasesNewestFirst(out)
	return out, nil
}

// Request is what the operator asked for.
type Request struct {
	// To pins an exact version. Empty means the newest release.
	To string
	// AllowPrerelease lets a prerelease be chosen as the newest. An explicit To always
	// wins over this: asking for a version by name is asking for it.
	AllowPrerelease bool
	// AllowDowngrade permits installing a version older than the running one (or older
	// than the highest ever verified here). It is off by default because a downgrade is
	// an attack far more often than it is an intention.
	AllowDowngrade bool
}

// Plan is a verified, ready-to-apply upgrade. Holding one means the provenance already
// checked out: the signature, the identity, and the transparency-log entry. What is
// left is the download, which is pinned to Digest, and the install.
type Plan struct {
	Current    Version
	Target     Version
	Provenance release.Provenance
	Asset      string
	Digest     string
	URL        string
	// Path is the binary that will be replaced: the running executable with its
	// symlinks resolved, which is not always the path the user typed.
	Path string
	// install carries the vetted destination. It is unexported so a caller cannot hand
	// Apply a destination that never went through resolveTarget's refusals.
	install installTarget
	// Downgrade is set when the target is older than the floor, which only a Request
	// that said so out loud can produce.
	Downgrade bool
	// Warning carries something the operator needs to read even though the plan is
	// valid, such as a listing that went backwards.
	Warning string
}

// UpToDate reports whether the plan would install what is already running.
func (p Plan) UpToDate() bool { return p.Current.Compare(p.Target) == 0 }

// Check verifies what the newest (or requested) release is and what installing it
// would mean, without installing anything and without writing to the binary's
// directory. It is what `flynn version check` and `flynn upgrade --check` run.
func (u *Updater) Check(ctx context.Context, req Request) (Plan, error) {
	cur, ok := ParseVersion(u.current())
	if !ok || isSourceBuild(u.current()) {
		// A binary built from a checkout has no release to be newer than, and no signed
		// provenance ever named it. Upgrading it would silently discard whatever local
		// build the developer is standing on.
		return Plan{}, fault.New(fault.Terminal, CodeDevBuild,
			"this flynn was built from source ("+u.current()+"), so there is no released version to upgrade from. "+
				"Install a released build to use `flynn upgrade`.")
	}

	releases, err := u.List(ctx)
	if err != nil {
		return Plan{}, err
	}

	st := loadState(u.dataDir)
	target, err := selectTarget(releases, req)
	if err != nil {
		return Plan{}, err
	}

	var warning string
	if prev, stale := st.staleListing(newestOf(releases)); stale {
		// This is not a reason to refuse: the operator may have legitimately been shown a
		// yanked release. It is a reason to say so loudly, because the alternative is a
		// machine that reports "up to date" while being held on an old version by someone
		// who controls its view of the world.
		warning = "the release listing no longer offers " + prev + ", which this machine has seen before. " +
			"That can mean a release was withdrawn, or that something is holding this machine back from newer ones."
	}

	floor := st.floor(cur)
	down := target.Compare(floor) < 0
	if down && !req.AllowDowngrade {
		return Plan{}, fault.New(fault.Terminal, CodeDowngrade,
			"refusing to install "+target.String()+", which is older than "+floor.String()+
				" (a downgrade to a version with known holes is the oldest trick there is). Pass --allow-downgrade if that is really what you want.")
	}

	// Everything above this line came from an unsigned listing. Everything below it is
	// checked against the trust root compiled into this binary.
	prov, err := u.provenance(ctx, target)
	if err != nil {
		return Plan{}, err
	}
	// The provenance is signed, but "signed" is not "signed for the thing I asked for":
	// a genuine bundle from a different release would otherwise let an attacker who can
	// swap assets serve v0.1.0's bundle in v0.9.0's place.
	if prov.Tag != target.String() {
		return Plan{}, fault.New(fault.Terminal, release.CodeUnexpectedTag,
			"the signed provenance served for "+target.String()+" is the provenance of "+prov.Tag)
	}

	asset := u.assetName()
	digest, err := prov.Digest(asset)
	if err != nil {
		return Plan{}, err
	}

	tgt, err := u.target()
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Current:    cur,
		Target:     target,
		Provenance: prov,
		Asset:      asset,
		Digest:     digest,
		URL:        u.downloadBase + target.String() + "/" + asset,
		Path:       tgt.Path,
		install:    tgt,
		Downgrade:  down,
		Warning:    warning,
	}, nil
}

// Apply installs a plan. It is separate from Check so that the command can show the
// operator exactly what verified, and what it is about to do, before it does it.
func (u *Updater) Apply(ctx context.Context, p Plan) error {
	// The archive is downloaded into the destination directory rather than the system
	// temp directory: the binary that comes out of it is renamed over the running one,
	// and a rename only works, let alone atomically, within a single filesystem.
	work, err := os.MkdirTemp(p.install.Dir, stagedPrefix+"work-*")
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	archive := filepath.Join(work, p.Asset)
	// The digest comes from the signed provenance, so this download either produces
	// exactly the bytes the flynn release workflow built, or it produces nothing.
	if _, err := u.fetcher.Fetch(ctx, fetch.Request{
		URL:          p.URL,
		Dest:         archive,
		ExpectSHA256: p.Digest,
		MaxBytes:     maxArchiveBytes,
	}); err != nil {
		return err
	}

	staged, err := p.install.stage(archive, u.binaryName())
	if err != nil {
		return err
	}

	// The binary is verified, so running it is safe, and running it is the only way to
	// find out that it runs: a correct download of a binary for the wrong libc, or one
	// the local security software will refuse to execute, is still a binary that will
	// not start. Better to find that out now, while the old one is still in place.
	if err := u.smokeTest(ctx, staged, p.Target); err != nil {
		_ = os.Remove(staged)
		return err
	}

	if err := p.install.replace(staged); err != nil {
		return err
	}

	// The floor only ever rises. Recording it after the install, rather than before,
	// means a failed upgrade cannot raise the bar for the next one.
	st := loadState(u.dataDir)
	if v, ok := ParseVersion(st.HighestVerified); !ok || p.Target.Compare(v) > 0 {
		st.HighestVerified = p.Target.String()
	}
	st.LastCheck = u.clock.Now().UTC()
	if err := saveState(u.dataDir, st); err != nil {
		// The binary is installed and working. Failing the command now would tell the
		// operator the upgrade did not happen, which would be false.
		return nil
	}
	return nil
}

// RecordSeen remembers the newest release the listing has offered, so a listing that
// later goes backwards can be noticed. It is called after a successful listing, and a
// failure to write it is not a failure of the command.
func (u *Updater) RecordSeen(releases []Release) {
	newest := newestOf(releases)
	st := loadState(u.dataDir)
	if v, ok := ParseVersion(st.NewestSeen); !ok || newest.Compare(v) > 0 {
		st.NewestSeen = newest.String()
	}
	st.LastCheck = u.clock.Now().UTC()
	_ = saveState(u.dataDir, st)
}

// smokeTest runs the staged binary and requires it to report the version it is
// supposed to be. Only a verified binary ever reaches this point, and it still runs
// through the sandbox boundary rather than with this process's ambient authority:
// a binary that was downloaded a second ago is exactly the kind of thing that should
// not be handed the agent's environment, its credentials, or its network, merely to
// answer what version it is.
func (u *Updater) smokeTest(ctx context.Context, staged string, want Version) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()

	sb, err := sandbox.NewLocal(filepath.Dir(staged), sandbox.WithDefaultConfinement())
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeSmokeTest, err)
	}
	defer func() { _ = sb.Close() }()

	res, err := sb.Capture(ctx, sandbox.CaptureSpec{Argv: []string{staged, "--version"}})
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeSmokeTest,
			fmt.Errorf("the new binary does not run on this machine, so it was not installed: %w", err))
	}
	if res.ExitCode != 0 {
		return fault.New(fault.Terminal, CodeSmokeTest,
			fmt.Sprintf("the new binary exited %d when asked for its version, so it was not installed", res.ExitCode))
	}
	out := []byte(res.Output)
	// The release build stamps its version without the tag's leading "v", so the strings
	// differ even when the versions do not. They are compared as versions for that
	// reason, not as text.
	got := strings.TrimSpace(string(out))
	reported, ok := ParseVersion(got)
	if !ok || reported.Compare(want) != 0 {
		return fault.New(fault.Terminal, CodeSmokeTest,
			"the new binary reports itself as "+got+" but was published as "+want.String()+", so it was not installed")
	}
	return nil
}

// provenance downloads a release's bundle and verifies it. The bytes are hostile until
// Verify says they are not.
func (u *Updater) provenance(ctx context.Context, v Version) (release.Provenance, error) {
	raw, err := u.get(ctx, u.downloadBase+v.String()+"/"+bundleAsset, maxBundleBytes)
	if err != nil {
		return release.Provenance{}, err
	}
	return release.Verify(raw)
}

// get reads a small document over the hardened transport.
func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fault.New(fault.Terminal, CodeListing, "refusing to fetch "+url+" over an unencrypted transport")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeListing, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fault.Wrap(fault.Transient, CodeListing, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fault.New(fault.Transient, CodeListing,
			fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url))
	}
	// Read one byte past the ceiling so an over-long body is refused rather than
	// truncated into something that parses as if it were complete.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, CodeListing, err)
	}
	if int64(len(raw)) > limit {
		return nil, fault.New(fault.Terminal, CodeListing,
			fmt.Sprintf("%s returned more than the %d-byte ceiling", url, limit))
	}
	return raw, nil
}

// selectTarget picks the version to install from what the listing offered.
func selectTarget(releases []Release, req Request) (Version, error) {
	if req.To != "" {
		want, ok := ParseVersion(req.To)
		if !ok {
			return Version{}, fault.New(fault.Terminal, CodeNoRelease, req.To+" is not a version")
		}
		for _, r := range releases {
			if r.Version.Compare(want) == 0 {
				return r.Version, nil
			}
		}
		return Version{}, fault.New(fault.Terminal, CodeNoRelease, "there is no release "+req.To)
	}
	for _, r := range releases {
		if r.Prerelease && !req.AllowPrerelease {
			continue
		}
		return r.Version, nil
	}
	return Version{}, fault.New(fault.Terminal, CodeNoRelease,
		"there are no stable releases yet; pass --pre to consider prereleases")
}

func newestOf(releases []Release) Version {
	var newest Version
	for _, r := range releases {
		if newest.raw == "" || r.Version.Compare(newest) > 0 {
			newest = r.Version
		}
	}
	return newest
}

func sortReleasesNewestFirst(rs []Release) {
	// Insertion sort: a release listing is a page of a hundred at most, and this keeps
	// the ordering in the same file as the comparison it depends on.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Version.Compare(rs[j-1].Version) > 0; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// assetName is the release archive for the platform this binary is running on. It is
// derived from this process's own GOOS and GOARCH and never from anything downloaded,
// so a hostile listing cannot talk this machine into installing a binary for another
// platform (or into naming a file it should not).
func (u *Updater) assetName() string {
	if u.goos == "windows" {
		return fmt.Sprintf("flynn_%s_%s.zip", u.goos, u.goarch)
	}
	return fmt.Sprintf("flynn_%s_%s.tar.gz", u.goos, u.goarch)
}

func (u *Updater) binaryName() string {
	if u.goos == "windows" {
		return "flynn.exe"
	}
	return "flynn"
}

// isSourceBuild reports whether a version string names a build that no release ever
// published: the "0.0.0-dev" default, or a version carrying a VCS revision. Both parse
// as versions, which is precisely the trap: left alone, a developer's own build would
// compare as an ancient prerelease and be "upgraded" out from under them.
func isSourceBuild(v string) bool {
	return v == "" || strings.HasPrefix(v, "0.0.0-dev") || strings.Contains(v, "+")
}

// current is the version this binary was built as. It is injectable so a test can
// stand in a released version without being built like one.
func (u *Updater) current() string {
	if u.version != "" {
		return u.version
	}
	return version.String()
}

func (u *Updater) target() (installTarget, error) {
	exe, err := u.exe()
	if err != nil {
		return installTarget{}, fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	return resolveTarget(exe)
}
