// Package selfupdate lets an installed flynn replace itself with a newer one, with no
// shell script, package manager or other tool.
//
// An update path is as secure as what it refuses to install. Everything fetched is
// hostile until the signed provenance checked by internal/release vouches for it, and
// the download is then pinned to that provenance. A compromised mirror, a hostile
// proxy, a rogue certificate authority or a GitHub fault serving the wrong bytes all
// end with a digest mismatch and nothing written.
//
// The release listing is the one unsigned input, used only to enumerate candidates.
// Whoever controls it can hide new versions or offer old ones, so the package
// remembers the highest version it verified and the newest it was offered, and moves
// below either only when explicitly asked.
//
// The install stages the new binary beside the one it replaces, so the swap is a
// rename within one filesystem; resolves the running binary's symlinks, so the write
// lands where aimed; refuses an install a package manager owns; and makes the new
// binary prove it runs before keeping it. Nothing unverified is executed.
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
	// smokeTestTimeout bounds the one run of the new binary. Printing a version is
	// instant, so a binary that cannot do it in this long is not installed.
	smokeTestTimeout = 30 * time.Second
)

// Updater upgrades the running binary.
type Updater struct {
	dataDir string
	http    *http.Client
	fetcher *fetch.Downloader
	clock   clock.Clock

	// These are injected so a test can run the whole path, install included, in a
	// temporary directory against a local server, because the disk-touching steps
	// carry the risk.
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

// Release is one release the listing offered. It is unverified: the tag is a
// candidate to check.
type Release struct {
	Version     Version
	Prerelease  bool
	PublishedAt time.Time
	// Current marks the release this binary is running.
	Current bool
}

// List reports the releases that exist, newest first. It verifies nothing: the listing
// only says what to check, and everything is verified before a byte is installed.
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
		// A tag that is not a version names nothing this binary can become.
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
	// wins over it.
	AllowPrerelease bool
	// AllowDowngrade permits a version older than the running one or than the highest
	// verified here. It is off by default because a downgrade is far more often an
	// attack than an intention.
	AllowDowngrade bool
}

// Plan is a verified upgrade ready to apply: the signature, identity and
// transparency-log entry have checked out. The download, pinned to Digest, and the
// install remain.
type Plan struct {
	Current    Version
	Target     Version
	Provenance release.Provenance
	Asset      string
	Digest     string
	URL        string
	// Path is the binary to replace: the running executable with its symlinks
	// resolved, which may differ from the path the user typed.
	Path string
	// install is unexported so a caller cannot hand Apply a destination that skipped
	// resolveTarget's refusals.
	install installTarget
	// Downgrade is set when the target is below the floor, which only a Request that
	// asked for it can produce.
	Downgrade bool
	// Warning is something the operator must read though the plan is valid, such as a
	// listing that went backwards.
	Warning string
}

// UpToDate reports whether the plan would install what is already running.
func (p Plan) UpToDate() bool { return p.Current.Compare(p.Target) == 0 }

// Check works out the newest or requested release and what installing it would mean,
// without installing or writing to the binary's directory. `flynn version check` and
// `flynn upgrade --check` run it.
func (u *Updater) Check(ctx context.Context, req Request) (Plan, error) {
	cur, ok := ParseVersion(u.current())
	if !ok || isSourceBuild(u.current()) {
		// A binary built from a checkout has no release to be newer than and no signed
		// provenance. Upgrading it would discard the developer's local build.
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
		// The operator may legitimately have been shown a yanked release, so this does
		// not refuse. It warns, because otherwise a machine held on an old version by
		// whoever controls its view would report "up to date".
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

	// Everything above came from the unsigned listing. Everything below is checked
	// against the trust root compiled into this binary.
	prov, err := u.provenance(ctx, target)
	if err != nil {
		return Plan{}, err
	}
	// A genuine bundle from a different release is still signed, so without this an
	// attacker who can swap assets could serve v0.1.0's bundle in v0.9.0's place.
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

// Apply installs a plan. It is separate from Check so the command can show the
// operator what verified and what it will do before doing it.
func (u *Updater) Apply(ctx context.Context, p Plan) error {
	// The archive is downloaded into the destination directory because the binary
	// from it is renamed over the running one, and a rename works, atomically, only
	// within one filesystem.
	work, err := os.MkdirTemp(p.install.Dir, stagedPrefix+"work-*")
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	archive := filepath.Join(work, p.Asset)
	// The digest comes from the signed provenance, so this download yields exactly the
	// bytes the release workflow built or nothing.
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
	// learn it runs: a binary for the wrong libc, or one local security software
	// refuses, will not start. Finding out now leaves the old one in place.
	if err := u.smokeTest(ctx, staged, p.Target); err != nil {
		_ = os.Remove(staged)
		return err
	}

	if err := p.install.replace(staged); err != nil {
		return err
	}

	// The floor only rises. It is recorded after the install, so a failed upgrade
	// cannot raise the bar for the next one.
	st := loadState(u.dataDir)
	if v, ok := ParseVersion(st.HighestVerified); !ok || p.Target.Compare(v) > 0 {
		st.HighestVerified = p.Target.String()
	}
	st.LastCheck = u.clock.Now().UTC()
	if err := saveState(u.dataDir, st); err != nil {
		// The binary is installed and working, so failing the command would wrongly
		// report that the upgrade did not happen.
		return nil
	}
	return nil
}

// RecordSeen remembers the newest release the listing offered, so a listing that later
// goes backwards is noticed. It runs after a successful listing, and a failed write
// does not fail the command.
func (u *Updater) RecordSeen(releases []Release) {
	newest := newestOf(releases)
	st := loadState(u.dataDir)
	if v, ok := ParseVersion(st.NewestSeen); !ok || newest.Compare(v) > 0 {
		st.NewestSeen = newest.String()
	}
	st.LastCheck = u.clock.Now().UTC()
	_ = saveState(u.dataDir, st)
}

// smokeTest runs the staged binary and requires it to report the version it should be.
// Only a verified binary gets here, and it still runs inside the sandbox boundary,
// because a binary downloaded a second ago should not get the agent's environment,
// credentials or network just to print its version.
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
	// The release build stamps its version without the tag's leading "v", so the two
	// are compared as versions, since the strings differ.
	got := strings.TrimSpace(string(out))
	reported, ok := ParseVersion(got)
	if !ok || reported.Compare(want) != 0 {
		return fault.New(fault.Terminal, CodeSmokeTest,
			"the new binary reports itself as "+got+" but was published as "+want.String()+", so it was not installed")
	}
	return nil
}

// provenance downloads a release's bundle and verifies it. The bytes are hostile until
// Verify accepts them.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
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
	// Read one byte past the ceiling so an over-long body is refused instead of
	// truncated into something that parses as complete.
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
	// Insertion sort: a listing page holds at most a hundred releases, and this keeps
	// the ordering beside the comparison it depends on.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Version.Compare(rs[j-1].Version) > 0; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// assetName is the release archive for this platform, derived from this process's
// GOOS and GOARCH and never from anything downloaded, so a hostile listing cannot get
// a binary for another platform installed, or name a file it should not.
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

// isSourceBuild reports whether v names a build no release published: the
// "0.0.0-dev" default or a version carrying a VCS revision. Both parse as versions, so
// without this a developer's build would compare as an ancient prerelease and be
// upgraded away.
func isSourceBuild(v string) bool {
	return v == "" || strings.HasPrefix(v, "0.0.0-dev") || strings.Contains(v, "+")
}

// current is the version this binary was built as. It is injectable so a test can use
// a released version.
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
